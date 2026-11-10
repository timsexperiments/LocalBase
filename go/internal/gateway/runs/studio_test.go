package runs

import (
	"net/http"
	"testing"
)

// An event stream from a target other than chat is that target's result: it
// used to be parsed as chat chunks, leaving a completed run with no result.
func TestRunMediaEventStreamIsItsResult(t *testing.T) {
	_, mgr := newRunHarness(t)
	m := mgr
	sse := "data: {\"type\":\"speech.audio.delta\",\"audio\":\"AAAA\"}\n\ndata: {\"type\":\"speech.audio.done\"}\n\n"
	r := &run{m: m, runRecord: runRecord{ID: "run_t", Target: "/v1/audio/speech", Status: runInProgress, Owner: "local"},
		notify: make(chan struct{}), done: make(chan struct{}), queuedSeen: true}
	rw := &runWriter{r: r, header: http.Header{"Content-Type": {"text/event-stream"}}}
	rw.WriteHeader(200)
	rw.Write([]byte(sse))
	m.finish(r, rw)
	if r.Status != runCompleted || string(r.result) != sse || r.resultType != "text/event-stream" || r.bytes != 0 {
		t.Errorf("status %s, result %q (%s), %d chunk bytes", r.Status, r.result, r.resultType, r.bytes)
	}
}
