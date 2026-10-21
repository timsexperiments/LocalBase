package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
)

// --- inference telemetry -------------------------------------------------------

// inference records one admitted request's content-free outcome as the
// `inference.completed` event (TS InferenceTelemetry).
type inference struct {
	s                      *Server
	modality, modelID      string
	spec                   *catalog.Spec
	requestID              string
	started                time.Time
	queueWait              time.Duration
	active, slots, waiting int
	streaming              bool
	prepareMs              *float64
	schemaValidation       string
	schemaSkipReasons      string
	upstreamStatus         int
	promptTokens           *float64
	completionTokens       *float64
	totalTokens            *float64
	promptMs, predictedMs  *float64
	finishReasons          map[string]bool
	finished               bool
}

var knownFinishReasons = []string{"stop", "length", "tool_calls", "content_filter", "function_call"}

func finiteNonNeg(v any) *float64 {
	f, ok := number(v)
	if !ok || f < 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return nil
	}
	return &f
}

// observe reads usage, timings and finish reasons from a validated backend
// body or stream event.
func (t *inference) observe(v map[string]any) {
	if choices, ok := v["choices"].([]any); ok {
		for _, c := range choices {
			ch, _ := c.(map[string]any)
			if fr, ok := ch["finish_reason"].(string); ok {
				if !contains(knownFinishReasons, fr) {
					fr = "unknown"
				}
				if t.finishReasons == nil {
					t.finishReasons = map[string]bool{}
				}
				t.finishReasons[fr] = true
			}
		}
	}
	if u, ok := v["usage"].(map[string]any); ok {
		if p := finiteNonNeg(u["prompt_tokens"]); p != nil {
			t.promptTokens = p
		}
		if p := finiteNonNeg(u["completion_tokens"]); p != nil {
			t.completionTokens = p
		}
		if p := finiteNonNeg(u["total_tokens"]); p != nil {
			t.totalTokens = p
		}
	}
	if tm, ok := v["timings"].(map[string]any); ok {
		if p := finiteNonNeg(tm["prompt_ms"]); p != nil {
			t.promptMs = p
		}
		if p := finiteNonNeg(tm["predicted_ms"]); p != nil {
			t.predictedMs = p
		}
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// serverTiming is the Server-Timing header for non-streaming responses.
func (t *inference) serverTiming() string {
	if t.streaming {
		return ""
	}
	var parts []string
	parts = append(parts, fmt.Sprintf("queue;dur=%.2f", ms(t.queueWait)))
	if t.prepareMs != nil {
		parts = append(parts, fmt.Sprintf("prepare;dur=%.2f", *t.prepareMs))
	}
	if t.promptMs != nil {
		parts = append(parts, fmt.Sprintf("prompt;dur=%.2f", *t.promptMs))
	}
	if t.predictedMs != nil {
		parts = append(parts, fmt.Sprintf("generation;dur=%.2f", *t.predictedMs))
	}
	return strings.Join(parts, ", ")
}

func (t *inference) finish(outcome string, status int, source string) {
	if t.finished {
		return
	}
	t.finished = true
	attrs := []any{"model_id", t.modelID}
	if t.spec != nil {
		rev := t.spec.RepositoryRevision
		var src struct{ Revision string }
		if json.Unmarshal(t.spec.Primary().Source, &src) == nil && src.Revision != "" {
			rev = src.Revision
		}
		attrs = append(attrs, "artifact_revision", rev, "quantization", t.spec.Quant)
	}
	attrs = append(attrs, "runtime_name", runtimeNames[t.modality], "admission_active", t.active,
		"admission_slots", t.slots, "admission_waiting", t.waiting, "outcome", outcome, "http_status", status)
	if t.upstreamStatus != 0 {
		attrs = append(attrs, "upstream_status", t.upstreamStatus)
	}
	attrs = append(attrs, "total_duration_ms", round2(ms(time.Since(t.started))), "queue_wait_ms", round2(ms(t.queueWait)))
	if source != "" {
		attrs = append(attrs, "terminal_source", source)
	}
	if t.prepareMs != nil && t.schemaValidation != "" {
		attrs = append(attrs, "json_schema_requested", true, "json_schema_native_mode", "requested",
			"json_schema_preparation_ms", round2(*t.prepareMs), "json_schema_validation", t.schemaValidation)
		if t.schemaSkipReasons != "" {
			attrs = append(attrs, "json_schema_skip_reasons", t.schemaSkipReasons)
		}
	}
	for _, f := range []struct {
		key string
		v   *float64
	}{{"prompt_tokens", t.promptTokens}, {"completion_tokens", t.completionTokens}, {"total_tokens", t.totalTokens},
		{"prompt_duration_ms", t.promptMs}, {"predicted_duration_ms", t.predictedMs}} {
		if f.v != nil {
			attrs = append(attrs, f.key, *f.v)
		}
	}
	if len(t.finishReasons) > 0 {
		reasons := sortedKeys(t.finishReasons)
		attrs = append(attrs, "finish_reasons", strings.Join(reasons, ","))
	}
	level := slog.LevelInfo
	switch outcome {
	case "error":
		level = slog.LevelError
	case "cancelled":
		level = slog.LevelWarn
	}
	t.s.Log.Log(context.Background(), level, "Inference response settled.", "event", "inference.completed",
		"category", "runtime", "component", "inference", "runtime", t.modality, "request_id", t.requestID,
		slog.Group("attributes", attrs...))
}

var runtimeNames = map[string]string{"llm": "llama-server", "stt": "whisper-server", "tts": "tts-server",
	"image": "sd-server", "video": "sd-server"}

// logAdmissionRejection records why a request was not admitted.
func (s *Server) logAdmissionRejection(requestID, modality, model string, err error) {
	var code, source string
	status := 0
	var ae *memory.AdmissionError
	switch {
	case errors.Is(err, runtime.ErrQueueFull):
		code, source, status = "inference_queue_full", "capacity", 429
	case errors.Is(err, runtime.ErrQueueTimeout):
		code, source, status = "inference_queue_timeout", "timeout", 429
	case errors.Is(err, runtime.ErrUnavailable):
		code, source, status = "service_unavailable", "unavailable", 503
	case errors.As(err, &ae):
		s.Log.Warn("Memory admission rejected the runtime.", "event", "inference.admission-rejected", "category", "runtime",
			"component", "inference", "runtime", modality, "request_id", requestID,
			slog.Group("attributes", "modality", modality, "model_id", model, "reason", ae.Reason,
				"requested_bytes", ae.RequestedBytes, "demand_confidence", ae.Confidence))
		return
	default:
		return
	}
	s.Log.Warn("Inference admission was rejected.", "event", "inference.admission-rejected", "category", "runtime",
		"component", "inference", "runtime", modality, "request_id", requestID,
		slog.Group("attributes", "modality", modality, "error_code", code, "source", source, "http_status", status))
}

// --- relay -------------------------------------------------------------------------

var proxyClient = &http.Client{Transport: &http.Transport{
	Proxy: nil, MaxIdleConnsPerHost: 16, IdleConnTimeout: 90 * time.Second, DisableCompression: true,
}}

func upstreamFailure(w http.ResponseWriter, r *http.Request, message string) {
	writeError(w, r, 502, "server_error", "upstream_error", message, nil)
}

// relay posts body to the backend and writes its response: event streams
// event by event, JSON after checking its shape. The request context cancels
// the backend call when the client goes away; the backend stays up (#276).
func (s *Server) relay(w http.ResponseWriter, r *http.Request, url string, body []byte, modelID string, o inferOpts, t *inference) {
	up, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(body))
	ct := "application/json"
	if o.contentType != "" {
		ct = o.contentType
	}
	up.Header.Set("Content-Type", ct)
	for _, h := range []string{"Traceparent", "Tracestate"} {
		if v := r.Header.Get(h); v != "" {
			up.Header.Set(h, v)
		}
	}
	resp, err := proxyClient.Do(up)
	if err != nil {
		if r.Context().Err() != nil {
			w.WriteHeader(499)
			t.finish("cancelled", 499, "request_aborted")
			return
		}
		upstreamFailure(w, r, "The upstream service could not be reached.")
		t.finish("error", 502, "")
		return
	}
	defer resp.Body.Close()
	t.upstreamStatus = resp.StatusCode
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	isSSE := strings.EqualFold(strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]), "text/event-stream")
	if ok && isSSE && o.kind == "chat" {
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		if v := resp.Header.Get("Cache-Control"); v != "" {
			w.Header().Set("Cache-Control", v)
		}
		w.WriteHeader(resp.StatusCode)
		outcome, source := relaySSE(r.Context(), w, resp.Body, modelID, t)
		t.finish(outcome, resp.StatusCode, source)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		if r.Context().Err() != nil {
			w.WriteHeader(499)
			t.finish("cancelled", 499, "request_aborted")
			return
		}
		upstreamFailure(w, r, "The upstream service returned malformed JSON.")
		t.finish("error", 502, "response_validation")
		return
	}
	if !ok {
		t.finish("error", s.relayUpstreamError(w, r, resp.StatusCode, raw), "")
		return
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var obj map[string]any
	if dec.Decode(&obj) != nil || obj == nil {
		upstreamFailure(w, r, "The upstream service returned malformed JSON.")
		t.finish("error", 502, "response_validation")
		return
	}
	if !validResponse(o.kind, obj) {
		upstreamFailure(w, r, "The upstream service returned an invalid response.")
		t.finish("error", 502, "response_validation")
		return
	}
	if o.schema != nil {
		outcome := structuredOutcome(obj, o.schema)
		t.schemaValidation = outcome
		t.schemaSkipReasons = skipReasons(obj)
		if outcome == "failed" {
			writeError(w, r, 502, "server_error", "structured_output_validation_failed",
				"The backend returned a completed response that did not match the requested JSON Schema.", nil)
			t.finish("error", 502, "response_validation")
			return
		}
	}
	if o.kind == "decision" {
		if u, ok := obj["usage"].(map[string]any); ok {
			in, _ := number(u["input_tokens"])
			out, _ := number(u["output_tokens"])
			t.observe(map[string]any{"usage": map[string]any{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out}})
		}
	} else {
		t.observe(obj)
	}
	if o.kind == "chat" {
		if choices, ok := obj["choices"].([]any); ok {
			for _, c := range choices {
				if ch, ok := c.(map[string]any); ok {
					if m, ok := ch["message"].(map[string]any); ok {
						normalizeReasoning(m)
					}
				}
			}
		}
	}
	if _, has := obj["model"]; has {
		obj["model"] = modelID // the public id, not the backend's file path
	}
	if st := t.serverTiming(); st != "" {
		w.Header().Set("Server-Timing", st)
	}
	writeJSON(w, resp.StatusCode, obj)
	t.finish("completed", resp.StatusCode, "")
}

// normalizeReasoning reports reasoning_content as `reasoning`, the field
// OpenAI-compatible clients read; reasoning_content itself is dropped.
func normalizeReasoning(m map[string]any) {
	rc, present := m["reasoning_content"]
	if !present {
		return
	}
	delete(m, "reasoning_content")
	if _, has := m["reasoning"]; !has && rc != nil {
		m["reasoning"] = rc
	}
}

func skipReasons(obj map[string]any) string {
	set := map[string]bool{}
	choices, _ := obj["choices"].([]any)
	for _, c := range choices {
		ch, _ := c.(map[string]any)
		msg, _ := ch["message"].(map[string]any)
		switch calls, _ := msg["tool_calls"].([]any); {
		case msg["refusal"] != nil:
			set["refusal"] = true
		case len(calls) > 0:
			set["tool_calls"] = true
		case ch["finish_reason"] == "length":
			set["truncation"] = true
		case ch["finish_reason"] == "content_filter":
			set["content_filter"] = true
		case ch["finish_reason"] != "stop":
			set["non_completed"] = true
		}
	}
	keys := sortedKeys(set)
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func isNum(v any) bool { _, ok := number(v); return ok }

// validResponse checks the shape TS validates with its response schemas.
func validResponse(kind string, obj map[string]any) bool {
	switch kind {
	case "chat":
		if !isString(obj["id"]) || !isString(obj["object"]) || !isNum(obj["created"]) || !isString(obj["model"]) {
			return false
		}
		choices, ok := obj["choices"].([]any)
		if !ok {
			return false
		}
		for _, c := range choices {
			ch, ok := c.(map[string]any)
			if !ok || !isNum(ch["index"]) {
				return false
			}
			if m, ok := ch["message"].(map[string]any); !ok || m["role"] != "assistant" {
				return false
			}
		}
	case "embeddings":
		data, ok := obj["data"].([]any)
		if !ok || !isString(obj["object"]) || !isString(obj["model"]) {
			return false
		}
		for _, d := range data {
			e, ok := d.(map[string]any)
			if !ok || !isString(e["object"]) || !isNum(e["index"]) {
				return false
			}
			if vec, ok := e["embedding"].([]any); !ok || !allNumbers(vec) {
				return false
			}
		}
		u, ok := obj["usage"].(map[string]any)
		return ok && isNum(u["prompt_tokens"]) && isNum(u["total_tokens"])
	case "image":
		// imageGenerationResponseSchema: created, and data items with b64_json.
		data, ok := obj["data"].([]any)
		if !ok || !isNum(obj["created"]) {
			return false
		}
		for _, d := range data {
			m, ok := d.(map[string]any)
			if !ok || !isString(m["b64_json"]) {
				return false
			}
			if rp, has := m["revised_prompt"]; has && !isString(rp) {
				return false
			}
		}
	case "decision":
		if !isString(obj["model"]) {
			return false
		}
		answers, ok := obj["answers"].(map[string]any)
		if !ok {
			return false
		}
		for _, a := range answers {
			ans, ok := a.(map[string]any)
			if !ok {
				return false
			}
			switch ans["type"] {
			case "choice":
				_, probs := ans["probabilities"].(map[string]any)
				if !isString(ans["choice"]) || !probs || !isNum(ans["confidence"]) {
					return false
				}
			case "score":
				_, probs := ans["probabilities"].(map[string]any)
				_, legend := ans["legend"].(map[string]any)
				if !isNum(ans["score"]) || !legend || !probs || !isNum(ans["confidence"]) {
					return false
				}
			case "noul":
				if !isNum(ans["noul"]) {
					return false
				}
			default:
				return false
			}
		}
		u, ok := obj["usage"].(map[string]any)
		return ok && isNum(u["input_tokens"]) && isNum(u["output_tokens"])
	}
	return true
}

func allNumbers(v []any) bool {
	for _, x := range v {
		if !isNum(x) {
			return false
		}
	}
	return true
}

// relayUpstreamError normalizes an OpenAI-shaped backend error. llama-server's
// context overflow becomes OpenAI's context_length_exceeded.
func (s *Server) relayUpstreamError(w http.ResponseWriter, r *http.Request, status int, raw []byte) int {
	var body struct {
		Error *struct {
			Message any `json:"message"`
			Type    any `json:"type"`
			Param   any `json:"param"`
			Code    any `json:"code"`
		} `json:"error"`
	}
	if status < 400 || json.Unmarshal(raw, &body) != nil || body.Error == nil || !nonEmptyString(body.Error.Message) || !nonEmptyString(body.Error.Type) {
		upstreamFailure(w, r, "The upstream service returned an invalid error response.")
		return 502
	}
	e := body.Error
	var param *string
	if p, ok := e.Param.(string); ok {
		param = &p
	}
	code := e.Code
	switch code.(type) {
	case string, float64, nil:
	default:
		code = nil
	}
	typ := e.Type.(string)
	if typ == "exceed_context_size_error" {
		writeError(w, r, 400, "invalid_request_error", "context_length_exceeded", e.Message.(string), strp("messages"))
		return 400
	}
	writeJSON(w, status, map[string]any{"error": apiError{Message: e.Message.(string), Type: typ, Param: param, Code: code}})
	return status
}

// --- SSE -----------------------------------------------------------------------------

const upstreamInvalidStream = `data: {"error":{"message":"The upstream service returned an invalid event stream.","type":"server_error","param":null,"code":"upstream_error"}}` + "\n\n"

func upstreamErrorEvent(message string) string {
	b, _ := json.Marshal(map[string]any{"error": apiError{Message: message, Type: "server_error", Code: "upstream_error"}})
	return "data: " + string(b) + "\n\n"
}

// relaySSE forwards a chat stream event by event, flushing each one. Data
// events get the public model id and `reasoning` normalization; an error
// event ends the stream; unparseable data ends it with upstream_error.
// Returns the inference outcome and terminal source.
func relaySSE(ctx context.Context, w http.ResponseWriter, body io.Reader, modelID string, t *inference) (string, string) {
	flusher, _ := w.(http.Flusher)
	rd := bufio.NewReaderSize(body, 64<<10)
	var event []string
	sawDone := false
	write := func(s string) bool {
		if _, err := io.WriteString(w, s); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	// emit returns (continue, outcome, source).
	emit := func() (bool, string, string) {
		if len(event) == 0 {
			return true, "", ""
		}
		var out strings.Builder
		for _, line := range event {
			data, isData := strings.CutPrefix(line, "data:")
			if !isData {
				out.WriteString(line + "\n")
				continue
			}
			data = strings.TrimPrefix(data, " ")
			if data == "[DONE]" {
				sawDone = true
				out.WriteString("data: [DONE]\n")
				continue
			}
			dec := json.NewDecoder(strings.NewReader(data))
			dec.UseNumber()
			var obj map[string]any
			if dec.Decode(&obj) != nil || obj == nil {
				write(upstreamInvalidStream)
				return false, "error", "response_validation"
			}
			if _, isErr := obj["error"]; isErr {
				write("data: " + data + "\n\n")
				return false, "error", "upstream_error_event"
			}
			t.observe(obj)
			if choices, ok := obj["choices"].([]any); ok {
				for _, c := range choices {
					if ch, ok := c.(map[string]any); ok {
						if d, ok := ch["delta"].(map[string]any); ok {
							normalizeReasoning(d)
						}
					}
				}
			}
			if _, has := obj["model"]; has {
				obj["model"] = modelID
			}
			b, _ := json.Marshal(obj)
			out.WriteString("data: " + string(b) + "\n")
		}
		event = event[:0]
		out.WriteString("\n")
		if !write(out.String()) {
			return false, "cancelled", "response_cancelled"
		}
		return true, "", ""
	}
	for {
		line, err := rd.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			if trimmed == "" && strings.HasSuffix(line, "\n") {
				if more, outcome, source := emit(); !more {
					return outcome, source
				}
			} else if strings.HasSuffix(line, "\n") {
				event = append(event, trimmed)
			} else {
				event = append(event, trimmed) // partial final line
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return "cancelled", "request_aborted"
			}
			if err != io.EOF {
				write(upstreamErrorEvent("The upstream stream failed."))
				return "error", "response_stream_error"
			}
			if len(event) > 0 {
				write(upstreamErrorEvent("The upstream stream ended mid-event."))
				return "error", "response_stream_error"
			}
			if !sawDone {
				write(upstreamInvalidStream)
				return "error", "response_validation"
			}
			return "completed", ""
		}
	}
}
