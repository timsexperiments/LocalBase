package httpx

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// stallingFlusher is a connection a client has stopped reading: a write fits in the buffer, and a
// flush waits out the write deadline and fails, as net/http's does.
type stallingFlusher struct {
	deadlineWriter
	deadline time.Time
}

func (s *stallingFlusher) SetWriteDeadline(t time.Time) error {
	s.deadline = t
	return s.deadlineWriter.SetWriteDeadline(t)
}

func (s *stallingFlusher) FlushError() error {
	time.Sleep(time.Until(s.deadline))
	return os.ErrDeadlineExceeded
}

func (s *stallingFlusher) Flush() { _ = s.FlushError() } // as net/http's: the error goes nowhere

// A flush that can't get out before the write deadline is a failed send, as a short write is. The
// stream was cut, so the request is a 499, not the 200 that went out first.
func TestAFlushThatStallsIsAFailedSend(t *testing.T) {
	s := &stallingFlusher{deadlineWriter: deadlineWriter{header: http.Header{}}}
	rec := &Recorder{ResponseWriter: s}
	rec.SendsWithin(50 * time.Millisecond)
	rec.WriteHeader(200)
	if _, err := rec.Write([]byte("data: {}\n\n")); err != nil {
		t.Fatal(err)
	}
	rec.Flush()
	if got := rec.FinalStatus(); got != 499 {
		t.Errorf("a stream cut at a flush came to %d, want 499", got)
	}
}

// Through net/http: a client that has stopped reading, and a stream that stops after a flush stalls,
// before writing again. That is the relay seeing the client's cancel before its next write would
// have failed. TestAClientThatStopsReadingLetsGo/streaming caught it once under load (run
// 42354799153), logged as a 200.
func TestAStreamCutAtAFlushIsA499(t *testing.T) {
	const stall = 300 * time.Millisecond
	final := make(chan int, 1)
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &Recorder{ResponseWriter: w}
		rec.SendsWithin(stall)
		defer func() { final <- rec.FinalStatus() }()
		rec.Header().Set("Content-Type", "text/event-stream")
		rec.WriteHeader(200)
		event := append(append([]byte("data: "), bytes.Repeat([]byte("x"), 1000)...), "\n\n"...)
		for range 1 << 16 { // up to 64 MiB, far past what the socket buffers hold
			if _, err := rec.Write(event); err != nil {
				return
			}
			began := time.Now()
			rec.Flush()
			if time.Since(began) >= stall-50*time.Millisecond {
				return // the flush waited out the deadline: the relay saw the cancel first
			}
		}
	}))
	ts.Start()
	defer ts.Close()
	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.(*net.TCPConn).SetReadBuffer(1024); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-final:
		if got != 499 {
			t.Errorf("a stream cut at a flush came to %d, want 499", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the stream never stalled")
	}
}
