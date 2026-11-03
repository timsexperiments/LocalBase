package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A chat run over what it may hold ends failed, with what it had, and the backend is told to stop.
func TestARunOverItsMemoryLimitFailsWithWhatItHad(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	s := runServer(ts)
	c := runClient{t: t, base: ts.URL}
	s.runs.maxHeld = 3000
	m := c.waitStatus(c.start("fake:slow 200"), "failed")
	if m["reason"] != "output_limit" || content(m) == "" {
		t.Errorf("%v", m)
	}
	if got := s.runs.held.Load(); got > 3000 {
		t.Errorf("holding %d, limit 3000", got)
	}
}

// All runs together have a limit too, with its own reason, and dropping a run gives its share back.
func TestRunsOverTheirTotalMemoryLimitFailAndGiveItBack(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	s := runServer(ts)
	c := runClient{t: t, base: ts.URL}
	s.runs.maxHeldTotal = 3000
	id := c.start("fake:slow 200")
	m := c.waitStatus(id, "failed")
	if m["reason"] != "memory_limit" || content(m) == "" {
		t.Errorf("%v", m)
	}
	if got := s.runs.held.Load(); got == 0 || got > 3000 {
		t.Errorf("holding %d, limit 3000", got)
	}
	if code, _ := c.do("DELETE", "/_localbase/runs/"+id, ""); code != 200 {
		t.Fatalf("dismissing: %d", code)
	}
	if got := s.runs.held.Load(); got != 0 {
		t.Errorf("holding %d after dismissing the only run", got)
	}
}

// The other ways a run goes give it back too.
func TestAnExpiredRunGivesBackWhatItHeld(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	s := runServer(ts)
	c := runClient{t: t, base: ts.URL}
	id := c.start("hello")
	c.waitStatus(id, "completed")
	if s.runs.held.Load() == 0 {
		t.Fatal("a finished run holds its output")
	}
	later := time.Now().Add(2 * runRetention)
	s.runs.now = func() time.Time { return later }
	s.runs.sweepExpired()
	if got := s.runs.held.Load(); got != 0 {
		t.Errorf("holding %d after it expired", got)
	}
}

// A media result counts while it arrives and stays counted if kept; a failure gives it back.
func TestAMediaBodyCountsAndAFailureGivesItBack(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	s := runServer(ts)
	m := s.runs
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	newRun := func() (*run, *runWriter) {
		r := &run{m: m, notify: make(chan struct{}), done: make(chan struct{}), cancel: cancel, queuedSeen: true}
		r.Target, r.ID = "/v1/images/generations", "run_test"
		return r, &runWriter{r: r, header: http.Header{}}
	}
	kept, w := newRun()
	w.WriteHeader(200)
	if _, err := w.Write(make([]byte, 1000)); err != nil {
		t.Fatal(err)
	}
	if got := m.held.Load(); got != 1000 {
		t.Errorf("holding %d while it arrives", got)
	}
	m.finish(kept, w)
	if got := m.held.Load(); got != 1000 || len(kept.result) != 1000 {
		t.Errorf("holding %d with a kept result of %d", got, len(kept.result))
	}
	m.release(kept)

	refused, w := newRun()
	w.WriteHeader(500)
	if _, err := w.Write([]byte(`{"error":{"message":"no"}}`)); err != nil {
		t.Fatal(err)
	}
	m.finish(refused, w)
	if got := m.held.Load(); got != 0 {
		t.Errorf("holding %d after a failure", got)
	}

	m.maxHeldTotal = 500
	over, w := newRun()
	w.WriteHeader(200)
	if _, err := w.Write(make([]byte, 1000)); err != nil {
		t.Fatal(err)
	}
	if over.stopStatus != runFailed || over.stopReason != "memory_limit" || w.body.Len() != 0 {
		t.Errorf("over the total: %q %q, %d kept", over.stopStatus, over.stopReason, w.body.Len())
	}
	if got := m.held.Load(); got != 0 {
		t.Errorf("holding %d after refusing the body", got)
	}
}

// Many tiny chunks are what the 8 MiB of payload doesn't see: their overhead is counted.
func TestTinyChunksCountTheirOverhead(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	s := runServer(ts)
	m := s.runs
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &run{m: m, notify: make(chan struct{}), done: make(chan struct{}), cancel: cancel}
	r.Target = chatTarget
	w := &runWriter{r: r, header: http.Header{}}
	m.maxHeld = 10 * (2*2 + runEventHeldFor)
	for range 11 {
		w.event("data: {}")
	}
	if len(r.events) != 10 || r.stopReason != "output_limit" {
		t.Errorf("%d events kept, reason %q", len(r.events), r.stopReason)
	}
}

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

func relayFor(t *testing.T, body io.Reader) (string, string, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	outcome, source := relaySSE(context.Background(), rec, body, "m", &inference{}, nil)
	return outcome, source, rec.Body.String()
}

func smallEventLimit(t *testing.T, limit int) {
	t.Helper()
	before := sseEventLimit
	sseEventLimit = limit
	t.Cleanup(func() { sseEventLimit = before })
}

// A line that never ends costs the limit and no more, and the client is told.
func TestAnEndlessUpstreamLineStopsAtTheLimit(t *testing.T) {
	smallEventLimit(t, 1<<20)
	body := &countingReader{}
	outcome, source, out := relayFor(t, body)
	if outcome != "error" || source != "response_event_limit" || !strings.Contains(out, "over the 1 MiB limit") {
		t.Errorf("%s %s %.200s", outcome, source, out)
	}
	if body.read > 1<<20+128<<10 {
		t.Errorf("read %d bytes of a line limited to 1 MiB", body.read)
	}
}

// The same limit covers an event of many short lines.
func TestAnEventOfManyLinesStopsAtTheLimit(t *testing.T) {
	smallEventLimit(t, 1000)
	lines := strings.Repeat(": comment\n", 200)
	outcome, source, _ := relayFor(t, strings.NewReader(lines))
	if outcome != "error" || source != "response_event_limit" {
		t.Errorf("%s %s", outcome, source)
	}
}

// A streaming client's request keeps nothing else: a stream many times the limit goes through.
func TestAStreamLongerThanTheLimitIsFine(t *testing.T) {
	smallEventLimit(t, 1000)
	event := `data: {"choices":[{"delta":{"content":"hello"}}]}` + "\n\n"
	body := strings.Repeat(event, 500) + "data: [DONE]\n\n"
	if len(body) < 10*1000 {
		t.Fatal("not longer than the limit")
	}
	outcome, source, out := relayFor(t, strings.NewReader(body))
	if outcome != "completed" || source != "" || strings.Count(out, "hello") != 500 {
		t.Errorf("%s %s, %d events", outcome, source, strings.Count(out, "hello"))
	}
}

// Exactly at the limit is allowed.
func TestALineAtTheLimitPasses(t *testing.T) {
	smallEventLimit(t, 128<<10)
	head := `data: {"choices":[{"delta":{"content":"`
	tail := `"}}]}` + "\n"
	line := head + strings.Repeat("x", 128<<10-len(head)-len(tail)-1) + tail
	body := line + "\n" + "data: [DONE]\n\n"
	outcome, source, _ := relayFor(t, strings.NewReader(body))
	if outcome != "completed" {
		t.Errorf("%s %s", outcome, source)
	}
}

// The flags reach the runs.
func TestRunMemoryLimitsAreConfigurable(t *testing.T) {
	s := &Server{RunMemoryLimit: 5 << 20, RunsMemoryLimit: 7 << 20}
	if m := newRunManager(s, ""); m.maxHeld != 5<<20 || m.maxHeldTotal != 7<<20 {
		t.Errorf("%d %d", m.maxHeld, m.maxHeldTotal)
	}
	if m := newRunManager(&Server{}, ""); m.maxHeld != runMaxHeld || m.maxHeldTotal != runsMaxHeld {
		t.Errorf("defaults: %d %d", m.maxHeld, m.maxHeldTotal)
	}
}

// A run refused before it queues leaves nothing counted.
func TestARefusedRunLeavesNothingCounted(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	s := runServer(ts)
	c := runClient{t: t, base: ts.URL}
	code, m := c.do("POST", "/_localbase/runs/v1/chat/completions", `{"messages":[]}`)
	if code != 400 || m["error"] == nil {
		t.Fatalf("not refused: %d %v", code, m)
	}
	if got := s.runs.held.Load(); got != 0 {
		t.Errorf("holding %d after a refusal", got)
	}
}

// Runs dropped for being over an owner's retained count give back what they held.
func TestATrimmedRunGivesBackWhatItHeld(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	m := runServer(ts).runs
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range runRetainedPerOwner + 1 {
		r := &run{m: m, notify: make(chan struct{}), done: make(chan struct{})}
		r.ID, r.Owner, r.Status = "run_"+strconv.Itoa(i), "me", runCompleted
		r.CreatedAt = now.Add(time.Duration(i) * time.Second)
		r.held = 100
		m.held.Add(100)
		m.runs[r.ID] = r
	}
	m.trim("me")
	if got := m.held.Load(); got != int64(runRetainedPerOwner*100) {
		t.Errorf("holding %d", got)
	}
}

// A run read back after a restart holds its output again, so it counts again.
func TestARestoredRunCounts(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	s := runServer(ts)
	dir := t.TempDir()
	first := newRunManager(s, dir)
	r := &run{m: first, notify: make(chan struct{}), done: make(chan struct{})}
	later := time.Now().Add(time.Hour)
	id := newRunID()
	r.Owner = "me"
	r.ID, r.Object, r.Target, r.Status, r.ExpiresAt = id, "localbase.run", chatTarget, runCompleted, &later
	r.output.Content = strings.Repeat("x", 5000)
	first.checkpoint(r)
	second := newRunManager(s, dir)
	second.mu.Lock()
	_, ok := second.runs[id]
	second.mu.Unlock()
	if !ok {
		t.Fatal("the run was not restored")
	}
	if got := second.held.Load(); got < 5000 {
		t.Errorf("holding %d for a restored output of 5000", got)
	}
}
