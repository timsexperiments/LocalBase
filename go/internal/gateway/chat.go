package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
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

func payloadTooLarge(w http.ResponseWriter, r *http.Request) {
	// The rest of the body may still be in flight: do not reuse the connection.
	w.Header().Set("Connection", "close")
	writeError(w, r, 413, "invalid_request_error", "payload_too_large", "Request body exceeds the 25 MiB limit.", nil)
}

// readJSON reads a bounded JSON object body, keeping unknown fields so they
// pass through to the backend unchanged.
func readJSON(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	if r.ContentLength > maxRequestBytes {
		payloadTooLarge(w, r)
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil {
		if r.Context().Err() != nil {
			w.WriteHeader(499)
			return nil, false
		}
		badRequest(w, r, "Invalid JSON payload.", nil)
		return nil, false
	}
	if len(body) > maxRequestBytes {
		payloadTooLarge(w, r)
		return nil, false
	}
	var req map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil || req == nil || dec.More() {
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

// prepareChat removes no-op tool fields, anchors tool schema patterns for
// llama.cpp's grammar converter, and turns a named tool_choice into
// "required" over that one tool.
func prepareChat(req map[string]any) {
	if tools, ok := req["tools"].([]any); ok && len(tools) == 0 {
		delete(req, "tools")
	}
	if fns, ok := req["functions"].([]any); ok && len(fns) == 0 {
		delete(req, "functions")
	}
	if tc, ok := req["tool_choice"]; ok && (tc == "none" || tc == nil) {
		delete(req, "tool_choice")
	}
	if fc, ok := req["function_call"]; ok && fc == "none" {
		delete(req, "function_call")
	}
	if msgs, ok := req["messages"].([]any); ok {
		for _, m := range msgs {
			if msg, ok := m.(map[string]any); ok && msg["role"] == "assistant" {
				if calls, ok := msg["tool_calls"].([]any); ok && len(calls) == 0 {
					delete(msg, "tool_calls")
				}
			}
		}
	}
	tools, _ := req["tools"].([]any)
	for _, t := range tools {
		fn, _ := t.(map[string]any)["function"].(map[string]any)
		if params, ok := fn["parameters"]; ok {
			fn["parameters"] = anchorPatterns(params)
		}
	}
	if tc, ok := req["tool_choice"].(map[string]any); ok {
		name := tc["function"].(map[string]any)["name"]
		var selected []any
		for _, t := range tools {
			if t.(map[string]any)["function"].(map[string]any)["name"] == name {
				selected = append(selected, t)
			}
		}
		req["tools"], req["tool_choice"] = selected, "required"
	}
}

var (
	dataKeywords      = []string{"enum", "const", "default", "examples"}
	schemaMapKeywords = []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"}
)

// anchorPatterns wraps unanchored `pattern` strings in ^...$; llama.cpp
// rejects unanchored ones while JSON Schema treats patterns as unanchored.
func anchorPatterns(v any) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = anchorPatterns(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			switch m, isMap := e.(map[string]any); {
			case k == "pattern":
				if s, ok := e.(string); ok {
					out[k] = anchorPattern(s)
				} else {
					out[k] = anchorPatterns(e)
				}
			case contains(dataKeywords, k):
				out[k] = e
			case contains(schemaMapKeywords, k) && isMap:
				inner := make(map[string]any, len(m))
				for name, schema := range m {
					inner[name] = anchorPatterns(schema)
				}
				out[k] = inner
			default:
				out[k] = anchorPatterns(e)
			}
		}
		return out
	}
	return v
}

func anchorPattern(p string) string {
	start := strings.HasPrefix(p, "^")
	end := strings.HasSuffix(p, "$") && !strings.HasSuffix(p, `\$`)
	if start && end {
		return p
	}
	body := p
	if strings.Contains(p, "|") {
		body = "(?:" + p + ")"
	}
	if !start {
		body = "^" + body
	}
	if !end {
		body += "$"
	}
	return body
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
	if c := validateChat(req); !c.ok() {
		c.reply(w, r)
		return
	}
	model := req["model"].(string)
	spec := s.Catalog.ByID(model)
	if p := toolParam(req); p != "" && spec != nil && !s.capabilities(r.Context(), spec).Has("tool-calling") {
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
	prepareChat(req)
	prepStart := time.Now()
	schema, rejection := prepareStructuredOutput(req["response_format"])
	prepMs := float64(time.Since(prepStart).Microseconds()) / 1000
	if rejection != nil {
		writeError(w, r, 400, "invalid_request_error", rejection.code, rejection.message, strp("response_format.json_schema.schema"))
		return
	}
	streaming := req["stream"] == true
	o := inferOpts{path: "/v1/chat/completions", kind: "chat", streaming: streaming, schema: schema}
	if schema != nil {
		o.prepareMs = &prepMs
	}
	_, a := req["max_tokens"]
	_, b := req["max_completion_tokens"]
	s.inferLLM(w, r, model, req, o, func(l leaseInfo) {
		if !a && !b {
			// No template-aware tokenizer here: keep the default within the
			// resolved per-slot context.
			limit := memory.ContextPerSequence(l.ctxSize, l.slots)
			if l.spec != nil && l.spec.ContextWindowTokens != nil {
				limit = min(limit, *l.spec.ContextWindowTokens)
			}
			req["max_tokens"] = min(DefaultMaxTokens(), limit, s.LLM.Config().CtxSize)
		}
	})
}

func (s *Server) embeddings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, "POST")
		return
	}
	req, ok := readJSON(w, r)
	if !ok {
		return
	}
	if c := validateEmbeddings(req); !c.ok() {
		c.reply(w, r)
		return
	}
	model := req["model"].(string)
	spec := s.Catalog.ByID(model)
	if spec != nil && spec.IsDecision() {
		badRequest(w, r, fmt.Sprintf("Model '%s' is a decision model. Send typed questions to POST /v1/systemone instead.", spec.ModelID), nil)
		return
	}
	if spec != nil && spec.IsEmbedding() && len(spec.LLMRuntime.Dimensions) > 0 {
		if v, present := req["dimensions"]; present {
			var d struct{ Minimum, Maximum float64 }
			json.Unmarshal(spec.LLMRuntime.Dimensions, &d)
			n, _ := number(v)
			if n < d.Minimum || n > d.Maximum {
				msg := fmt.Sprintf("Model '%s' supports dimensions from %s through %s.", model, trimFloat(d.Minimum), trimFloat(d.Maximum))
				if d.Minimum == d.Maximum {
					msg = fmt.Sprintf("Model '%s' supports %s dimensions.", model, trimFloat(d.Minimum))
				}
				badRequest(w, r, msg, nil)
				return
			}
		}
	}
	s.inferLLM(w, r, model, req, inferOpts{path: "/v1/embeddings", kind: "embeddings"}, nil)
}

func (s *Server) systemone(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, "POST")
		return
	}
	req, ok := readJSON(w, r)
	if !ok {
		return
	}
	if c := validateDecision(req); !c.ok() {
		c.reply(w, r)
		return
	}
	model := req["model"].(string)
	if spec := s.Catalog.ByID(model); spec != nil && !spec.IsDecision() {
		writeError(w, r, 400, "invalid_request_error", "unsupported_model_capability",
			fmt.Sprintf(`Model '%s' is not a decision model. Choose a model whose architecture.output_modalities include "decisions" from GET /v1/models.`, spec.ModelID), strp("model"))
		return
	}
	// A missing or blank instructions defaults to the question id.
	for id, q := range req["questions"].(map[string]any) {
		qm := q.(map[string]any)
		if in, present := qm["instructions"]; !present || (isString(in) && strings.TrimSpace(in.(string)) == "") {
			qm["instructions"] = id
		}
	}
	s.inferLLM(w, r, model, req, inferOpts{path: "/v1/systemone", kind: "decision"}, nil)
}

func isString(v any) bool { _, ok := v.(string); return ok }

// leaseInfo is what a handler may use to finish the backend request once
// the model is admitted.
type leaseInfo struct {
	spec    *catalog.Spec
	ctxSize int
	slots   int
}

type inferOpts struct {
	path      string
	kind      string // chat | embeddings | decision
	streaming bool
	schema    map[string]any
	prepareMs *float64
}

// inferLLM admits the request on the LLM runtime, finishes the body, and
// relays the backend response. The lease is held until the body is relayed.
func (s *Server) inferLLM(w http.ResponseWriter, r *http.Request, model string, req map[string]any, o inferOpts, finish func(leaseInfo)) {
	started := time.Now()
	requestID := w.Header().Get("X-Localbase-Request-Id")
	if _, _, err := s.LLM.Resolve(model); err != nil {
		modelNotFound(w, r, model)
		return
	}
	lease, err := s.LLM.Acquire(r.Context(), model)
	if err != nil {
		s.logAdmissionRejection(requestID, "llm", model, err)
		runtimeError(w, r, err, model)
		return
	}
	defer lease.Release()
	if finish != nil {
		finish(leaseInfo{spec: lease.Spec, ctxSize: lease.Plan.CtxSize, slots: lease.Plan.Parallel.Slots})
	}
	req["model"] = lease.ModelID
	body, _ := json.Marshal(req)
	tel := &inference{s: s, modality: "llm", modelID: lease.ModelID, spec: lease.Spec, requestID: requestID,
		started: started, queueWait: lease.Waited, active: lease.Active, slots: lease.Slots, waiting: lease.Waiting,
		streaming: o.streaming, prepareMs: o.prepareMs}
	if o.schema != nil {
		tel.schemaValidation = "not_performed"
		if o.streaming {
			tel.schemaValidation = "not_performed_streaming"
		}
	}
	s.relay(w, r, lease.BaseURL+o.path, body, lease.ModelID, o, tel)
}
