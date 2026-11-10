package infer

import (
	"context"
	"io"
	"testing"
	"time"
)

// The relay's stream reader, on its own: the HTTP tests of its limits are in relay_bounds_test.

// No limit before a stream's first byte, in the reader itself.
func TestAnIdleStreamWaitsForItsFirstByte(t *testing.T) {
	// The same once the answer has begun but before any of its body has come.
	pr, pw := io.Pipe()
	cancelled := make(chan struct{})
	stream := &idleReader{r: pr, limit: 100 * time.Millisecond, cancel: func() { close(cancelled); pw.CloseWithError(context.Canceled) }}
	go func() {
		time.Sleep(400 * time.Millisecond)
		pw.Write([]byte("data: [DONE]\n\n"))
		pw.Close()
	}()
	b, err := io.ReadAll(stream)
	stream.stop()
	if err != nil || string(b) != "data: [DONE]\n\n" {
		t.Errorf("a body whose first byte came late: %q %v", b, err)
	}
	select {
	case <-cancelled:
		t.Error("the call was cancelled before its first byte")
	default:
	}
}

// The idle limit reads as a person would say it.
func TestTheIdleLimitReads(t *testing.T) {
	if got := idleText(5 * time.Minute); got != "5 minutes" {
		t.Errorf("the limit reads %q", got)
	}
}
