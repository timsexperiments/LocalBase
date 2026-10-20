package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

const maxRequestBytes = 25 << 20

// DefaultMaxTokens applies when a chat request sets neither max_tokens nor
// max_completion_tokens; LOCALBASE_DEFAULT_MAX_TOKENS overrides it.
func DefaultMaxTokens() int {
	if v, err := strconv.Atoi(os.Getenv("LOCALBASE_DEFAULT_MAX_TOKENS")); err == nil && v > 0 {
		return v
	}
	return 4096
}

// readJSON reads a bounded JSON object body, keeping unknown fields so they
// pass through to the backend unchanged.
func readJSON(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	if r.ContentLength > maxRequestBytes {
		writeError(w, r, 413, "invalid_request_error", "payload_too_large", "Request body exceeds the 25 MiB limit.", nil)
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil {
		badRequest(w, r, "Could not read the request body.", nil)
		return nil, false
	}
	if len(body) > maxRequestBytes {
		writeError(w, r, 413, "invalid_request_error", "payload_too_large", "Request body exceeds the 25 MiB limit.", nil)
		return nil, false
	}
	var req map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil || req == nil {
		badRequest(w, r, "Invalid JSON payload.", nil)
		return nil, false
	}
	return req, true
}

// toolParam names the first field that asks for tool calling, or "".
func toolParam(req map[string]any) string {
	if tools, ok := req["tools"].([]any); ok && len(tools) > 0 {
		return "tools"
	}
	if tc, ok := req["tool_choice"]; ok && tc != "none" {
		return "tool_choice"
	}
	if fns, ok := req["functions"].([]any); ok && len(fns) > 0 {
		return "functions"
	}
	if fc, ok := req["function_call"]; ok && fc != "none" {
		return "function_call"
	}
	msgs, _ := req["messages"].([]any)
	for i, m := range msgs {
		msg, _ := m.(map[string]any)
		if msg["role"] == "tool" {
			return fmt.Sprintf("messages[%d]", i)
		}
		if calls, ok := msg["tool_calls"].([]any); ok && len(calls) > 0 && msg["role"] == "assistant" {
			return fmt.Sprintf("messages[%d].tool_calls", i)
		}
	}
	return ""
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, "POST")
		return
	}
	req, ok := readJSON(w, r)
	if !ok {
		return
	}
	model, _ := req["model"].(string)
	if strings.TrimSpace(model) == "" {
		badRequest(w, r, "model must not be empty", strp("model"))
		return
	}
	if _, ok := req["messages"].([]any); !ok {
		badRequest(w, r, "messages must be an array", strp("messages"))
		return
	}
	id, spec, err := s.LLM.Resolve(model)
	if err != nil {
		modelNotFound(w, r, model)
		return
	}
	if p := toolParam(req); p != "" && spec != nil && !spec.HasCapability("tool-calling") {
		writeError(w, r, 400, "invalid_request_error", "unsupported_model_capability",
			fmt.Sprintf("Model '%s' does not support tool calling. Choose a tool-capable model from GET /v1/models.", spec.ModelID), strp(p))
		return
	}
	if spec != nil && spec.LLMRuntime != nil {
		msg := fmt.Sprintf("Model '%s' supports embeddings only.", spec.ModelID)
		if spec.IsDecision() {
			msg = fmt.Sprintf("Model '%s' is a decision model. Send typed questions to POST /v1/systemone instead.", spec.ModelID)
		}
		badRequest(w, r, msg, nil)
		return
	}

	lease, err := s.LLM.Acquire(r.Context(), id)
	if err != nil {
		var ae *memory.AdmissionError
		if errors.As(err, &ae) {
			s.Log.Warn("Memory admission rejected the runtime.", "event", "inference.admission-rejected", "runtime", "llm",
				"request_id", w.Header().Get("X-Localbase-Request-Id"), "model_id", id, "reason", ae.Reason,
				"requested_bytes", ae.RequestedBytes, "demand_confidence", ae.Confidence)
		}
		runtimeError(w, r, err, model)
		return
	}
	defer lease.Release() // held until the body is fully relayed

	if _, a := req["max_tokens"]; !a {
		if _, b := req["max_completion_tokens"]; !b {
			limit := memory.ContextPerSequence(lease.Plan.CtxSize, lease.Plan.Parallel.Slots)
			if lease.Spec != nil && lease.Spec.ContextWindowTokens != nil {
				limit = min(limit, *lease.Spec.ContextWindowTokens)
			}
			req["max_tokens"] = min(DefaultMaxTokens(), limit, s.LLM.Config().CtxSize)
		}
	}
	req["model"] = id
	body, _ := json.Marshal(req)
	s.proxy(w, r, lease.BaseURL+"/v1/chat/completions", body, id)
}

// proxy relays a backend response, flushing each chunk so SSE streams stay
// incremental. The request context cancels the upstream call when the
// client goes away; llama-server stays up (#276).
func (s *Server) proxy(w http.ResponseWriter, r *http.Request, url string, body []byte, modelID string) {
	up, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(body))
	up.Header.Set("Content-Type", "application/json")
	for _, h := range []string{"Traceparent", "Tracestate"} {
		if v := r.Header.Get(h); v != "" {
			up.Header.Set(h, v)
		}
	}
	resp, err := http.DefaultClient.Do(up)
	if err != nil {
		if r.Context().Err() != nil {
			w.WriteHeader(499)
			return
		}
		serviceUnavailable(w, r, "LLM")
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		w.WriteHeader(resp.StatusCode)
		relaySSE(w, resp.Body, modelID)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		serviceUnavailable(w, r, "LLM")
		return
	}
	// Report the public model id, not the backend's file path.
	if resp.StatusCode < 300 {
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) == nil && obj != nil {
			obj["model"], _ = json.Marshal(modelID)
			raw, _ = json.Marshal(obj)
		}
	}
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	w.Write(raw)
}

const upstreamErrorEvent = "data: {\"error\":{\"message\":\"The upstream service returned an invalid event stream.\",\"type\":\"server_error\",\"param\":null,\"code\":\"upstream_error\"}}\n\n"

// relaySSE forwards a chat stream event by event, flushing each one. JSON
// data events get the public model id; anything unparseable ends the stream
// with an upstream_error event.
func relaySSE(w http.ResponseWriter, body io.Reader, modelID string) {
	flusher, _ := w.(http.Flusher)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var event []string
	flush := func() bool {
		if len(event) == 0 {
			return true
		}
		var out strings.Builder
		for _, line := range event {
			data, isData := strings.CutPrefix(line, "data:")
			if isData {
				data = strings.TrimPrefix(data, " ")
				if data != "[DONE]" {
					var obj map[string]json.RawMessage
					if json.Unmarshal([]byte(data), &obj) != nil {
						w.Write([]byte(upstreamErrorEvent))
						return false
					}
					if _, has := obj["model"]; has {
						obj["model"], _ = json.Marshal(modelID)
					}
					b, _ := json.Marshal(obj)
					data = string(b)
				}
				line = "data: " + data
			}
			out.WriteString(line + "\n")
		}
		out.WriteString("\n")
		event = event[:0]
		if _, err := w.Write([]byte(out.String())); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if line == "" {
			if !flush() {
				return
			}
			continue
		}
		event = append(event, line)
	}
	flush()
}
