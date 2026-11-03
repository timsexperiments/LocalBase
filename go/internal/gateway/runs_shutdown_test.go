package gateway

import (
	"testing"
	"time"
)

// A run published just as the gateway shuts down must be stopped by it. It used
// to get its cancel and its place in the wait group only after being published,
// so Shutdown skipped it and it went on generating with no one waiting for it.
func TestRunCreatedDuringShutdownIsStopped(t *testing.T) {
	ts := newTestServer(t)
	s := runServer(ts)
	c := runClient{t: t, base: ts.URL}
	shutdown := false
	s.runs.afterPublish = func() {
		shutdown = true
		s.runs.Shutdown(50 * time.Millisecond)
	}
	code, m := c.do("POST", "/_localbase/runs/v1/chat/completions", chatBody("fake:slow 5"))
	if !shutdown {
		t.Fatalf("the shutdown never ran: %d %v", code, m)
	}
	if code != 202 {
		return // refused before the queue: nothing is left running
	}
	m = c.waitStatus(m["id"].(string), "interrupted")
	if m["reason"] != "gateway_restart" {
		t.Errorf("run after shutdown: %v", m)
	}
}

// Shutdown waits for a run published before it, however early that run is.
func TestShutdownWaitsForRunJustPublished(t *testing.T) {
	ts := newTestServer(t)
	s := runServer(ts)
	c := runClient{t: t, base: ts.URL}
	finished := make(chan struct{})
	s.runs.afterPublish = func() {
		go func() {
			s.runs.Shutdown(30 * time.Second)
			close(finished)
		}()
		select {
		case <-finished:
			t.Error("Shutdown returned with a published run not finished")
		case <-time.After(200 * time.Millisecond):
		}
	}
	c.do("POST", "/_localbase/runs/v1/chat/completions", chatBody("fake:slow 5"))
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Error("Shutdown never returned")
	}
}
