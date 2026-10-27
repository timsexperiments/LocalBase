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
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/otlp"
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
	// span is the localbase.inference span and ctx carries it.
	span *otlp.Span
	ctx  context.Context
	// hooks tell a detached run (§21) how the request ended.
	hooks *runHooks
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

// begin starts the inference span under the request's server span, with
// the admission facts (TS InferenceTelemetry constructor).
func (t *inference) begin(ctx context.Context, method string) {
	if t.ctx != nil {
		return
	}
	t.ctx, t.span = t.s.Otel.Start(ctx, "localbase.inference", otlp.KindInternal, "http.request.method", method)
	sp := t.span
	sp.SetAttr("localbase.inference.model_id", t.modelID)
	sp.SetAttr("localbase.inference.runtime.name", runtimeNames[t.modality])
	if t.spec != nil {
		sp.SetAttr("localbase.inference.artifact.revision", t.revision())
		sp.SetAttr("localbase.inference.model.quantization", t.spec.Quant)
	}
	sp.SetAttr("localbase.inference.admission.active", t.active)
	sp.SetAttr("localbase.inference.admission.slots", t.slots)
	sp.SetAttr("localbase.inference.admission.waiting", t.waiting)
	sp.SetAttr("localbase.request_id", t.requestID)
	sp.SetAttr("localbase.inference.queue.duration_ms", ms(t.queueWait))
	if t.prepareMs != nil {
		sp.SetAttr("localbase.inference.json_schema.requested", true)
		sp.SetAttr("localbase.inference.json_schema.native_mode", "requested")
		sp.SetAttr("localbase.inference.json_schema.preparation.duration_ms", *t.prepareMs)
	}
}

// revision is the artifact revision the model was installed from.
func (t *inference) revision() string {
	rev := t.spec.RepositoryRevision
	var src struct{ Revision string }
	if json.Unmarshal(t.spec.Primary().Source, &src) == nil && src.Revision != "" {
		rev = src.Revision
	}
	return rev
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
	t.hooks.finish(source)
	attrs := []any{"model_id", t.modelID}
	if t.spec != nil {
		attrs = append(attrs, "artifact_revision", t.revision(), "quantization", t.spec.Quant)
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
	t.finishSpan(outcome, status, source)
	level := slog.LevelInfo
	switch outcome {
	case "error":
		level = slog.LevelError
	case "cancelled":
		level = slog.LevelWarn
	}
	ctx := t.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	defer t.span.End()
	t.s.Log.Log(ctx, level, "Inference response settled.", "event", "inference.completed",
		"category", "runtime", "component", "inference", "runtime", t.modality, "request_id", t.requestID,
		slog.Group("attributes", attrs...))
}

// finishSpan records the outcome on the inference span (TS finish).
func (t *inference) finishSpan(outcome string, status int, source string) {
	sp := t.span
	sp.SetAttr("localbase.inference.total.duration_ms", ms(time.Since(t.started)))
	sp.SetAttr("localbase.inference.outcome", outcome)
	sp.SetAttr("http.response.status_code", status)
	if t.upstreamStatus != 0 {
		sp.SetAttr("localbase.inference.upstream.status_code", t.upstreamStatus)
	}
	if source != "" {
		sp.SetAttr("localbase.inference.terminal.source", source)
	}
	if t.prepareMs != nil && t.schemaValidation != "" {
		sp.SetAttr("localbase.inference.json_schema.validation", t.schemaValidation)
		if t.schemaSkipReasons != "" {
			sp.SetAttr("localbase.inference.json_schema.skip_reasons", t.schemaSkipReasons)
		}
	}
	for _, f := range []struct {
		key string
		v   *float64
	}{{"gen_ai.usage.input_tokens", t.promptTokens}, {"gen_ai.usage.output_tokens", t.completionTokens},
		{"localbase.inference.usage.total_tokens", t.totalTokens}, {"localbase.inference.backend.prompt.duration_ms", t.promptMs},
		{"localbase.inference.backend.predicted.duration_ms", t.predictedMs}} {
		if f.v != nil {
			sp.SetAttr(f.key, *f.v)
		}
	}
	if len(t.finishReasons) > 0 {
		sp.SetAttr("localbase.inference.finish_reasons", strings.Join(sortedKeys(t.finishReasons), ","))
	}
	if outcome != "completed" {
		sp.SetError("")
	}
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
	t.begin(r.Context(), r.Method)
	up, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(body))
	ct := "application/json"
	if o.contentType != "" {
		ct = o.contentType
	}
	up.Header.Set("Content-Type", ct)
	resp, err := s.backendCall(t.ctx, r, up, proxyClient)
	if err != nil {
		if r.Context().Err() != nil {
			w.WriteHeader(499)
			t.finish("cancelled", 499, "request_aborted")
			return
		}
		if o.interrupted != nil && o.interrupted() {
			memoryInterrupted(w, r, t)
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
		outcome, source := relaySSE(r.Context(), w, resp.Body, modelID, t, o.interrupted)
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
		if o.interrupted != nil && o.interrupted() {
			memoryInterrupted(w, r, t)
			return
		}
		upstreamFailure(w, r, "The upstream service returned malformed JSON.")
		t.finish("error", 502, "response_validation")
		return
	}
	if !ok && o.interrupted != nil && o.interrupted() {
		memoryInterrupted(w, r, t)
		return
	}
	if !ok {
		t.finish("error", s.relayUpstreamError(w, r, resp.StatusCode, raw, o.kind), "")
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

// backendCall sends the backend request inside a localbase.backend.inference
// client span, carrying its trace context to the backend. Without an
// exporter the caller's own trace context is passed through.
func (s *Server) backendCall(ctx context.Context, r *http.Request, up *http.Request, client *http.Client) (*http.Response, error) {
	if !s.Otel.Enabled() {
		for _, h := range []string{"Traceparent", "Tracestate"} {
			if v := r.Header.Get(h); v != "" {
				up.Header.Set(h, v)
			}
		}
		return client.Do(up)
	}
	ctx, span := s.Otel.Start(ctx, "localbase.backend.inference", otlp.KindClient,
		"server.address", up.URL.Hostname(), "http.request.method", r.Method)
	defer span.End()
	otlp.Inject(ctx, up.Header)
	resp, err := client.Do(up)
	if err != nil {
		span.RecordException("Error", "The upstream service could not be reached.")
		span.SetError("The upstream service could not be reached.")
		return nil, err
	}
	span.SetAttr("http.response.status_code", resp.StatusCode)
	if resp.StatusCode >= 500 {
		span.SetError("Upstream returned a server error.")
	}
	return resp, nil
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
// context overflow becomes OpenAI's context_length_exceeded, and so does an
// input over its physical batch size, which it reports as a 500
// server_error (Go only, DESIGN §13.6 item 7).
func (s *Server) relayUpstreamError(w http.ResponseWriter, r *http.Request, status int, raw []byte, kind string) int {
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
	if msg := e.Message.(string); typ == "server_error" && batchOverflowRe.MatchString(msg) {
		writeError(w, r, 400, "invalid_request_error", "context_length_exceeded", batchOverflowMessage(msg, kind), strp(inputParam(kind)))
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
func relaySSE(ctx context.Context, w http.ResponseWriter, body io.Reader, modelID string, t *inference, interrupted func() bool) (string, string) {
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
			if interrupted != nil && interrupted() {
				write(memoryEmergencyEvent)
				return "error", "memory_emergency"
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

// memoryEmergencyEvent ends a stream whose runtime was stopped because memory
// turned critical: the service is unavailable, and the client may retry.
const memoryEmergencyEvent = `data: {"error":{"message":"Inference service is currently restarting or unavailable. Please try again shortly.","type":"api_error","param":null,"code":"service_unavailable"}}` + "\n\n"

// memoryInterrupted answers a request whose runtime was stopped because
// memory turned critical: 503 with Retry-After, not a truncated response
// (a Go difference: TS answers 502 upstream_error, DESIGN §13.6).
func memoryInterrupted(w http.ResponseWriter, r *http.Request, t *inference) {
	runtimeError(w, r, runtime.ErrMemoryEmergency, "", "Inference")
	t.finish("error", 503, "memory_emergency")
}

// llama-server's error for an input over the physical batch size (-b/-ub):
// "input (27634 tokens) is too large to process. increase the physical
// batch size (current batch size: 8192)".
var (
	batchOverflowRe = regexp.MustCompile(`(?i)is too large to process\b.*\bbatch size`)
	batchInputRe    = regexp.MustCompile(`input \((\d+) tokens\)`)
	batchLimitRe    = regexp.MustCompile(`batch size: (\d+)`)
)

// batchOverflowMessage states the limit in product words.
func batchOverflowMessage(msg, kind string) string {
	model := map[string]string{"decision": "this decision model", "embeddings": "this embedding model"}[kind]
	if model == "" {
		model = "this model"
	}
	in, lim := batchInputRe.FindStringSubmatch(msg), batchLimitRe.FindStringSubmatch(msg)
	if in == nil || lim == nil {
		return "The input is larger than " + model + " accepts at once. Shorten it and try again."
	}
	return fmt.Sprintf("The input is about %s tokens, but %s accepts up to %s.", groupDigits(in[1]), model, groupDigits(lim[1]))
}

// inputParam is the request field that carries the input.
func inputParam(kind string) string {
	switch kind {
	case "decision":
		return "state"
	case "embeddings":
		return "input"
	}
	return "messages"
}

// groupDigits writes 27634 as 27,634.
func groupDigits(n string) string {
	for i := len(n) - 3; i > 0; i -= 3 {
		n = n[:i] + "," + n[i:]
	}
	return n
}
