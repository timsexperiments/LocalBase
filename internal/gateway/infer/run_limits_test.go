package infer

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	goruntime "runtime"
	"strings"
	"testing"
)

// countingReader is a line far longer than any limit here, counting how much was asked for. It ends
// at 64 MiB, so that a relay with no limit fails this test rather than filling the memory.
type countingReader struct{ read int }

func (c *countingReader) Read(p []byte) (int, error) {
	if c.read >= 64<<20 {
		return 0, io.EOF
	}
	for i := range p {
		p[i] = 'x'
	}
	c.read += len(p)
	return len(p), nil
}

func relayFor(t *testing.T, body io.Reader, eventLimit int) (string, string, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	outcome, source := relaySSE(context.Background(), rec, body, "m", &inference{}, nil, eventLimit)
	return outcome, source, rec.Body.String()
}

// A line that never ends costs the limit and no more, and the client is told.
func TestAnEndlessUpstreamLineStopsAtTheLimit(t *testing.T) {
	limit := 1 << 20
	body := &countingReader{}
	outcome, source, out := relayFor(t, body, limit)
	if outcome != "error" || source != "response_event_limit" || !strings.Contains(out, "over the 1 MiB limit") {
		t.Errorf("%s %s %.200s", outcome, source, out)
	}
	if body.read > 1<<20+128<<10 {
		t.Errorf("read %d bytes of a line limited to 1 MiB", body.read)
	}
}

// The same limit covers an event of many short lines.
func TestAnEventOfManyLinesStopsAtTheLimit(t *testing.T) {
	limit := 1000
	lines := strings.Repeat(": comment\n", 200)
	outcome, source, _ := relayFor(t, strings.NewReader(lines), limit)
	if outcome != "error" || source != "response_event_limit" {
		t.Errorf("%s %s", outcome, source)
	}
}

// A streaming client's request keeps nothing else: a stream many times the limit goes through.
func TestAStreamLongerThanTheLimitIsFine(t *testing.T) {
	limit := 1000
	event := `data: {"choices":[{"delta":{"content":"hello"}}]}` + "\n\n"
	body := strings.Repeat(event, 500) + "data: [DONE]\n\n"
	if len(body) < 10*1000 {
		t.Fatal("not longer than the limit")
	}
	outcome, source, out := relayFor(t, strings.NewReader(body), limit)
	if outcome != "completed" || source != "" || strings.Count(out, "hello") != 500 {
		t.Errorf("%s %s, %d events", outcome, source, strings.Count(out, "hello"))
	}
}

// Exactly at the limit is allowed.
func TestALineAtTheLimitPasses(t *testing.T) {
	limit := 128 << 10
	head := `data: {"choices":[{"delta":{"content":"`
	tail := `"}}]}` + "\n"
	line := head + strings.Repeat("x", 128<<10-len(head)-len(tail)-1) + tail
	body := line + "\n" + "data: [DONE]\n\n"
	outcome, source, _ := relayFor(t, strings.NewReader(body), limit)
	if outcome != "completed" {
		t.Errorf("%s %s", outcome, source)
	}
}

// discardWriter is a client that reads and keeps nothing, counting what it was sent, and measures the
// heap as it goes. A recorder would keep the stream and measure itself. Each sample collects first,
// so that it is what is still held: other tests in the package can leave garbage, and the collector
// lets that grow with the heap.
type discardWriter struct {
	header   http.Header
	sent     int64
	writes   int
	peakLive uint64
	peakHeap uint64
}

func (d *discardWriter) Header() http.Header { return d.header }
func (d *discardWriter) WriteHeader(int)     {}
func (d *discardWriter) Write(b []byte) (int, error) {
	d.sent += int64(len(b))
	d.writes++
	var ms goruntime.MemStats
	if d.writes%64 == 0 {
		goruntime.ReadMemStats(&ms)
		d.peakHeap = max(d.peakHeap, ms.HeapAlloc)
	}
	if d.writes%4096 == 0 {
		goruntime.GC()
		goruntime.ReadMemStats(&ms)
		d.peakLive = max(d.peakLive, ms.HeapAlloc)
	}
	return len(b), nil
}

// eventReader is a stream of n chat events, generated, so that the test itself holds none of it.
type eventReader struct {
	event []byte
	left  int
	rest  []byte
}

func (e *eventReader) Read(p []byte) (int, error) {
	if len(e.rest) == 0 {
		if e.left == 0 {
			e.rest = []byte("data: [DONE]\n\n")
			e.left = -1
		} else if e.left < 0 {
			return 0, io.EOF
		} else {
			e.rest = e.event
			e.left--
		}
	}
	n := copy(p, e.rest)
	e.rest = e.rest[n:]
	return n, nil
}

// The number behind "a streaming client's request doesn't accumulate": the heap during a 256 MiB
// stream, sixteen times the 16 MiB event limit.
func TestAStreamingRequestsHeapDoesNotGrowWithTheStream(t *testing.T) {
	event := []byte(`data: {"choices":[{"delta":{"content":"` + strings.Repeat("x", 4<<10) + `"}}]}` + "\n\n")
	const total = 256 << 20
	n := total / len(event)
	goruntime.GC()
	var base goruntime.MemStats
	goruntime.ReadMemStats(&base)
	w := &discardWriter{header: http.Header{}}
	outcome, source := relaySSE(context.Background(), w, &eventReader{event: event, left: n}, "m", &inference{}, nil, sseEventLimit)
	if outcome != "completed" {
		t.Fatalf("%s %s", outcome, source)
	}
	if w.sent < total*9/10 { // The relay rewrites each event, so not to the byte.
		t.Fatalf("sent %d bytes", w.sent)
	}
	growth := int64(w.peakLive) - int64(base.HeapAlloc)
	t.Logf("a %d MiB stream: %d KiB held at the start, at most %d KiB held during, growth %d KiB; heap with garbage at most %d KiB",
		w.sent>>20, base.HeapAlloc>>10, w.peakLive>>10, growth>>10, w.peakHeap>>10)
	if growth > 8<<20 {
		t.Errorf("what is held grew %d MiB during a %d MiB stream", growth>>20, w.sent>>20)
	}
}
