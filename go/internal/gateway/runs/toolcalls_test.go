package runs

import (
	"encoding/json"
	"testing"
)

func toolDelta(index any, id, args string) map[string]any {
	call := map[string]any{"id": id, "function": map[string]any{"name": "", "arguments": args}}
	if index != nil {
		call["index"] = index
	}
	return map[string]any{"choices": []any{map[string]any{"index": float64(0),
		"delta": map[string]any{"tool_calls": []any{call}}}}}
}

// The index comes from the backend, which a custom model can make anything. It
// must not decide how much the gateway allocates, or panic it.
func TestRunToolCallIndexBounded(t *testing.T) {
	for name, index := range map[string]any{
		"huge":       float64(1_000_000),
		"past limit": float64(runMaxToolCalls),
		"negative":   float64(-1),
		"fractional": 1.5,
	} {
		t.Run(name, func(t *testing.T) {
			var o runOutput
			o.add(toolDelta(index, "x", "{}"))
			if len(o.ToolCalls) != 0 {
				t.Errorf("%d tool calls from index %v", len(o.ToolCalls), index)
			}
		})
	}
	var o runOutput
	o.add(toolDelta(nil, "first", ""))                         // no index: the first call, as before
	o.add(toolDelta(float64(runMaxToolCalls-1), "last", "{}")) // the last slot still works
	if len(o.ToolCalls) != runMaxToolCalls || o.ToolCalls[0]["id"] != "first" || o.ToolCalls[runMaxToolCalls-1]["id"] != "last" {
		t.Errorf("%d calls: %v, %v", len(o.ToolCalls), o.ToolCalls[0], o.ToolCalls[len(o.ToolCalls)-1])
	}
}

// A snapshot is encoded after the run's lock is released, while deltas keep
// arriving. It must share nothing that they change.
func TestRunViewOwnsItsToolCalls(t *testing.T) {
	r := &run{}
	r.Target = chatTarget
	r.output.add(toolDelta(float64(0), "a", `{"x"`))
	v := r.view(true)
	before, err := json.Marshal(v["output"])
	if err != nil {
		t.Fatal(err)
	}
	r.output.add(toolDelta(float64(0), "b", `:1}`))
	after, err := json.Marshal(v["output"])
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("the snapshot changed:\n%s\n%s", before, after)
	}
}

// The same with both at once, for -race: an encoder on a shared map is a fatal
// "concurrent map read and map write", which no recovery catches.
func TestRunViewConcurrentWithDeltas(t *testing.T) {
	r := &run{}
	r.Target = chatTarget
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			r.mu.Lock()
			r.output.add(toolDelta(float64(i%4), "id", "x"))
			r.mu.Unlock()
		}
	}()
	for {
		select {
		case <-done:
			return
		default:
		}
		r.mu.Lock()
		v := r.view(true)
		r.mu.Unlock()
		if _, err := json.Marshal(v); err != nil {
			t.Fatal(err)
		}
	}
}
