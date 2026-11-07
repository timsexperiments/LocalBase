package gateway

import (
	"testing"
	"time"
)

// A bad cursor is the caller's mistake, answered before the stream starts. It
// used to slice r.events[-1:] with the run's lock held: the middleware recovered
// the panic, the lock stayed taken, and the run and the sweep behind it hung.
func TestRunEventsBadCursor(t *testing.T) {
	ts := newTestServer(t)
	c := RunClient{T: t, Base: ts.URL}
	id := c.Start("fake:slow 3")
	for _, tc := range []struct{ name, query, header string }{
		{"negative after", "?after=-1", ""},
		{"negative header", "", "-1"},
		{"text after", "?after=x", ""},
		{"text header", "", "x"},
		{"overflow", "?after=99999999999999999999", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hdr []string
			if tc.header != "" {
				hdr = []string{"Last-Event-ID", tc.header}
			}
			if code, m := c.Do("GET", "/_localbase/runs/"+id+"/events"+tc.query, "", hdr...); code != 400 {
				t.Errorf("status %d: %v", code, m)
			}
		})
	}
	// The run was not harmed: its lock is free and it finishes.
	done := make(chan map[string]any, 1)
	go func() { done <- c.WaitStatus(id, "completed") }()
	select {
	case m := <-done:
		if Content(m) != Words(3) {
			t.Errorf("output %q", Content(m))
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the run hung after a bad cursor")
	}
}

// Whatever goes wrong while reading events, the lock is not left taken.
func TestRunEventsAfterReleasesTheLock(t *testing.T) {
	r := &run{}
	func() {
		defer func() { _ = recover() }()
		r.eventsAfter(-1)
	}()
	if !r.mu.TryLock() {
		t.Fatal("the run's lock is still taken")
	}
	r.mu.Unlock()
}
