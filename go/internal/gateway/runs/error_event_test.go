package runs

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
)

// errorEvent is the data of the run's one error event, read from its event stream once the run ends.
func errorEvent(t *testing.T, c gatewaytest.RunClient, id string) map[string]any {
	t.Helper()
	evs, _, _ := c.Events(id, nil, "", nil)
	var found []map[string]any
	for _, e := range evs {
		if e.Name != "error" {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(e.Data), &data); err != nil {
			t.Fatalf("error event isn't JSON: %q", e.Data)
		}
		found = append(found, data)
	}
	if len(found) != 1 {
		t.Fatalf("%d error events: %v", len(found), evs)
	}
	return found[0]
}

// A backend's error event, as llama-server sends it, reaches a run's event stream as the run's read
// gives it: in the same {"error": …} envelope, with each field only as its own kind, each text
// bounded and redacted, and the backend's own fields left out. So the stream keeps the read's promise.
func TestARunsErrorEventIsTheBoundedError(t *testing.T) {
	ts, _ := newRunHarness(t)
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	long := "Bearer sk-abcdefghijklmnopqrstuvwxyz0123456789 " + strings.Repeat("x", 5000)
	id := c.Start(streamErr(`{"error":{"code":400,"type":"exceed_context_size_error","message":"` + long + `","n_prompt_tokens":5000,"n_ctx":4096,"param":7}}`))
	own := c.WaitStatus(id, "failed")
	data := errorEvent(t, c, id)
	if len(data) != 1 {
		t.Errorf("the event has more than its error: %v", data)
	}
	ev, _ := data["error"].(map[string]any)
	read, _ := own["error"].(map[string]any)
	msg, _ := ev["message"].(string)
	if n := utf8.RuneCountInString(msg); n > maxListErrorText || strings.Contains(msg, "sk-abcdefghij") {
		t.Errorf("the event's message: %d characters, %.50q", n, msg)
	}
	if ev["code"] != float64(400) || ev["type"] != "exceed_context_size_error" || len(ev) != 3 {
		t.Errorf("the event's error: %v", ev)
	}
	for k, v := range read {
		if ev[k] != v {
			t.Errorf("%s: the event has %v, the read %v", k, ev[k], v)
		}
	}
}

// A streamed error with nothing LocalBase keeps says why in the event too.
func TestARunsErrorEventWithNothingToKeepSaysWhy(t *testing.T) {
	ts, _ := newRunHarness(t)
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	id := c.Start(streamErr(`{"error":{"detail":"x"}}`))
	c.WaitStatus(id, "failed")
	ev, _ := errorEvent(t, c, id)["error"].(map[string]any)
	if ev["code"] != "upstream_error" || ev["message"] != "The run ended without a result." {
		t.Errorf("the event: %v", ev)
	}
}

// An oversized backend error event adds only the bounded envelope to what the run holds, and what
// the manager holds: the run keeps that envelope and the bounded error, not the backend's bytes.
func TestAnOversizedErrorEventHoldsOnlyTheBoundedEnvelope(t *testing.T) {
	ts, mgr := newRunHarness(t)
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	huge := strings.Repeat("x", 200<<10) // 200 KiB in the message, and as much in a field of its own
	id := c.Start(streamErr(`{"error":{"code":400,"type":"exceed_context_size_error","message":"` + huge + `","detail":"` + huge + `"}}`))
	c.WaitStatus(id, "failed")
	mgr.mu.Lock()
	r := mgr.runs[id]
	mgr.mu.Unlock()
	r.mu.Lock()
	held, kept, bytes := r.held, r.Error, r.bytes
	var events int
	for _, e := range r.events {
		if e.name == "error" {
			events += len(e.data)
		}
	}
	r.mu.Unlock()
	envelope, _ := json.Marshal(map[string]any{"error": kept})
	if events != len(envelope) {
		t.Errorf("the error event is %d bytes, the bounded envelope %d", events, len(envelope))
	}
	// What a run holds besides its error is its output: here only the stream's first chunk, counted
	// as every chunk is (twice its bytes, kept as an event and in the output, and the event's own share).
	if held != int64(len(envelope))+2*int64(bytes)+runEventHeldFor {
		t.Errorf("the run holds %d bytes: the envelope is %d and its one chunk %d", held, len(envelope), bytes)
	}
	if held > 4<<10 || mgr.held.Load() > 4<<10 {
		t.Errorf("the run holds %d and the manager %d for a %d-byte backend event", held, mgr.held.Load(), 400<<10)
	}
	if msg, _ := kept["message"].(string); utf8.RuneCountInString(msg) > maxListErrorText {
		t.Errorf("the run keeps a %d-character message", utf8.RuneCountInString(msg))
	}
	if _, ok := kept["detail"]; ok {
		t.Error("the run keeps the backend's own field")
	}
}
