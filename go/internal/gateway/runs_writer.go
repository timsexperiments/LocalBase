package gateway

import (
	"bytes"
	"encoding/json"
	"maps"
	"math"
	"net/http"
	"strings"
)

// runWriter receives the inner /v1 response: an event stream becomes run
// events and accumulated output; anything else is kept for the error.
type runWriter struct {
	r      *run
	header http.Header
	status int
	sse    bool
	buf    []byte
	body   bytes.Buffer
}

func (w *runWriter) Header() http.Header { return w.header }

func (w *runWriter) WriteHeader(code int) {
	if w.status != 0 {
		return
	}
	w.status = code
	ct := strings.TrimSpace(strings.SplitN(w.header.Get("Content-Type"), ";", 2)[0])
	// Only chat streams become chunks; any other target's body is its result,
	// whatever its type (speech can answer as SSE).
	w.sse = w.r.Target == chatTarget && code >= 200 && code < 300 && strings.EqualFold(ct, "text/event-stream")
}

func (w *runWriter) Flush() {}

func (w *runWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	if !w.sse {
		m := w.r.m
		if w.body.Len()+len(b) > m.maxResult {
			m.stop(w.r, runFailed, "output_limit")
			return len(b), nil
		}
		if m.held.Add(int64(len(b))) > m.maxHeldTotal {
			m.held.Add(-int64(len(b)))
			m.stop(w.r, runFailed, "memory_limit")
			return len(b), nil
		}
		w.r.mu.Lock()
		w.r.held += int64(len(b))
		w.r.mu.Unlock()
		w.body.Write(b)
		return len(b), nil
	}
	w.buf = append(w.buf, b...)
	for {
		i := bytes.Index(w.buf, []byte("\n\n"))
		if i < 0 {
			break
		}
		block := string(w.buf[:i])
		w.buf = w.buf[i+2:]
		w.event(block)
	}
	return len(b), nil
}

// event takes one relayed SSE event: a chunk, [DONE], or an error.
func (w *runWriter) event(block string) {
	var data []string
	for _, line := range strings.Split(block, "\n") {
		if d, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(d, " "))
		}
	}
	payload := strings.Join(data, "\n")
	if payload == "" || payload == "[DONE]" {
		return
	}
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return
	}
	r := w.r
	m := r.m
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := obj["error"].(map[string]any); ok {
		r.Error = e
		r.held += int64(len(payload))
		m.held.Add(int64(len(payload)))
		r.emit("error", []byte(payload))
		return
	}
	stop := func(reason string) {
		if r.stopStatus == "" {
			r.stopStatus, r.stopReason = runFailed, reason
			r.cancel()
		}
	}
	r.bytes += len(payload)
	if r.bytes > m.maxOutput {
		stop("output_limit")
		return
	}
	// Twice: it is kept as an event and folded into the output, which can't grow by more than it.
	if reason := m.charge(r, 2*int64(len(payload))+runEventHeldFor); reason != "" {
		stop(reason)
		return
	}
	r.output.add(obj)
	r.emit("chunk", []byte(payload))
	if m.now().Sub(r.saved) >= runCheckpointEvery {
		m.checkpoint(r)
	}
}

// snapshot copies what add changes in place, so the copy can be encoded after
// the run's lock is released. Usage is replaced whole, never changed.
func (o *runOutput) snapshot() *runOutput {
	out := *o
	if o.ToolCalls != nil {
		out.ToolCalls = make([]map[string]any, len(o.ToolCalls))
		for i, call := range o.ToolCalls {
			c := maps.Clone(call)
			if fn, ok := call["function"].(map[string]any); ok {
				c["function"] = maps.Clone(fn)
			}
			out.ToolCalls[i] = c
		}
	}
	return &out
}

// add folds one chat.completion.chunk into the output.
func (o *runOutput) add(chunk map[string]any) {
	if u, ok := chunk["usage"].(map[string]any); ok {
		o.Usage = u
	}
	choices, _ := chunk["choices"].([]any)
	for _, c := range choices {
		ch, _ := c.(map[string]any)
		if idx, _ := ch["index"].(float64); idx != 0 {
			continue
		}
		if fr, ok := ch["finish_reason"].(string); ok {
			o.FinishReason = fr
		}
		d, _ := ch["delta"].(map[string]any)
		if s, ok := d["content"].(string); ok {
			o.Content += s
		}
		if s, ok := d["reasoning"].(string); ok {
			o.Reasoning += s
		}
		calls, _ := d["tool_calls"].([]any)
		for _, tc := range calls {
			call, _ := tc.(map[string]any)
			i := 0
			if f, ok := call["index"].(float64); ok {
				// The backend chooses this number: a delta that does not name a
				// real slot is left out of the output (its chunk is still relayed).
				if f < 0 || f >= runMaxToolCalls || f != math.Trunc(f) {
					continue
				}
				i = int(f)
			}
			for len(o.ToolCalls) <= i {
				o.ToolCalls = append(o.ToolCalls, map[string]any{"type": "function", "function": map[string]any{"name": "", "arguments": ""}})
			}
			dst := o.ToolCalls[i]
			if id, ok := call["id"].(string); ok && id != "" {
				dst["id"] = id
			}
			fn, _ := call["function"].(map[string]any)
			dfn := dst["function"].(map[string]any)
			if n, ok := fn["name"].(string); ok {
				dfn["name"] = dfn["name"].(string) + n
			}
			if a, ok := fn["arguments"].(string); ok {
				dfn["arguments"] = dfn["arguments"].(string) + a
			}
		}
	}
}
