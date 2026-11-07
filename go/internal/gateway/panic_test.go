package gateway

import (
	"errors"
	"io"
	"net/http"
	"testing"
)

// A handler that panics after its answer has started must not look finished: the connection is
// reset, so a client never takes half a stream for the whole of it. Before the first byte, an
// ordinary panic still answers 503, and http.ErrAbortHandler aborts as net/http defines it.
func TestPanicAfterTheFirstByteResetsTheConnection(t *testing.T) {
	ts := newTestServer(t)
	s := runServer(ts)
	s.mux.HandleFunc("GET /test/panic-mid-stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"partial\":true}\n\n"))
		w.(http.Flusher).Flush()
		panic("boom")
	})
	resp, err := http.Get(ts.URL + "/test/panic-mid-stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Errorf("a stream that panicked half way ended cleanly, as if complete: %q", b)
	}
}

func TestAbortHandlerPanicAborts(t *testing.T) {
	ts := newTestServer(t)
	s := runServer(ts)
	s.mux.HandleFunc("GET /test/abort", func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
	resp, err := http.Get(ts.URL + "/test/abort")
	if err == nil {
		resp.Body.Close()
		t.Errorf("http.ErrAbortHandler answered %d; it should abort the connection", resp.StatusCode)
	}
}

func TestPanicBeforeAnyWriteIsA503(t *testing.T) {
	ts := newTestServer(t)
	s := runServer(ts)
	s.mux.HandleFunc("GET /test/panic-early", func(http.ResponseWriter, *http.Request) { panic(errors.New("boom")) })
	resp, err := http.Get(ts.URL + "/test/panic-early")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
		t.Errorf("a panic before any write: %d, Retry-After %q; want 503 with Retry-After", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}
