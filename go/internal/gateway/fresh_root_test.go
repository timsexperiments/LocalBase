package gateway

import (
	"slices"
	"testing"
	"time"
)

// A configured default is not held until a request starts loading it, and it is
// held while it loads, before the runtime is up: an install must not race a load.
func TestHeldModels(t *testing.T) {
	t.Setenv("FAKE_STARTUP_MS", "800")
	ts, llm, _ := newTestGateway(t, host{})
	if got := llm.LoadedOrLoading(); len(got) != 0 {
		t.Fatalf("idle: %v", got)
	}
	if id := llm.Snapshot().ModelID; id == "" {
		t.Fatal("the test needs an active model")
	}
	done := make(chan result, 1)
	go func() { done <- post(t, ts, "/v1/chat/completions", chatBody("hi")) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(llm.LoadedOrLoading()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("never loading")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if id, _, _ := llm.Running(); id != "" {
		t.Errorf("already running (%s): the startup delay did not apply", id)
	}
	if !slices.Contains(llm.LoadedOrLoading(), chatModel) {
		t.Errorf("loading: %v", llm.LoadedOrLoading())
	}
	if r := <-done; r.status != 200 {
		t.Fatalf("chat: %d %s", r.status, r.body)
	}
	if !slices.Contains(llm.LoadedOrLoading(), chatModel) {
		t.Errorf("running: %v", llm.LoadedOrLoading())
	}
}
