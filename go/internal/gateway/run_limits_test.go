package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A chat run over what it may hold ends failed, with what it had, and the backend is told to stop.
func TestARunOverItsMemoryLimitFailsWithWhatItHad(t *testing.T) {
	ts, mgr := newRunHarness(t)
	c := runClient{t: t, base: ts.URL}
	mgr.maxHeld = 3000
	m := c.waitStatus(c.start("fake:slow 200"), "failed")
	if m["reason"] != "output_limit" || content(m) == "" {
		t.Errorf("%v", m)
	}
	if got := mgr.held.Load(); got > 3000 {
		t.Errorf("holding %d, limit 3000", got)
	}
}

// All runs together have a limit too, with its own reason, and dropping a run gives its share back.
func TestRunsOverTheirTotalMemoryLimitFailAndGiveItBack(t *testing.T) {
	ts, mgr := newRunHarness(t)
	c := runClient{t: t, base: ts.URL}
	mgr.maxHeldTotal = 3000
	id := c.start("fake:slow 200")
	m := c.waitStatus(id, "failed")
	if m["reason"] != "memory_limit" || content(m) == "" {
		t.Errorf("%v", m)
	}
	if got := mgr.held.Load(); got == 0 || got > 3000 {
		t.Errorf("holding %d, limit 3000", got)
	}
	e, _ := m["error"].(map[string]any)
	if e == nil || e["code"] != CodeMemoryFull || !strings.Contains(e["message"].(string), "wait for the running ones") {
		t.Errorf("nothing else had finished: %v", e)
	}
	if code, _ := c.do("DELETE", "/_localbase/runs/"+id, ""); code != 200 {
		t.Fatalf("dismissing: %d", code)
	}
	if got := mgr.held.Load(); got != 0 {
		t.Errorf("holding %d after dismissing the only run", got)
	}
}

// The other ways a run goes give it back too.
func TestAnExpiredRunGivesBackWhatItHeld(t *testing.T) {
	ts, mgr := newRunHarness(t)
	c := runClient{t: t, base: ts.URL}
	id := c.start("hello")
	c.waitStatus(id, "completed")
	if mgr.held.Load() == 0 {
		t.Fatal("a finished run holds its output")
	}
	later := time.Now().Add(2 * runRetention)
	mgr.now = func() time.Time { return later }
	mgr.sweepExpired()
	if got := mgr.held.Load(); got != 0 {
		t.Errorf("holding %d after it expired", got)
	}
}

// A media result counts while it arrives and stays counted if kept; a failure gives it back.
func TestAMediaBodyCountsAndAFailureGivesItBack(t *testing.T) {
	_, mgr := newRunHarness(t)
	m := mgr
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
	_, mgr := newRunHarness(t)
	m := mgr
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

// The flags reach the runs' options, and unset they leave them unset, for the defaults.
func TestRunMemoryLimitsAreConfigurable(t *testing.T) {
	s := &Server{RunMemoryLimit: 5 << 20, RunsMemoryLimit: 7 << 20}
	if o := s.runOptions(""); o.MaxHeld != 5<<20 || o.MaxHeldTotal != 7<<20 {
		t.Errorf("%d %d", o.MaxHeld, o.MaxHeldTotal)
	}
	if o := (&Server{}).runOptions(""); o.MaxHeld != 0 || o.MaxHeldTotal != 0 {
		t.Errorf("defaults: %d %d", o.MaxHeld, o.MaxHeldTotal)
	}
}

// The limits in the options are the runs' limits, and unset they are the defaults.
func TestRunMemoryLimitsAreTheOptions(t *testing.T) {
	if m := NewManager(Options{MaxHeld: 5 << 20, MaxHeldTotal: 7 << 20}); m.maxHeld != 5<<20 || m.maxHeldTotal != 7<<20 {
		t.Errorf("%d %d", m.maxHeld, m.maxHeldTotal)
	}
	if m := NewManager(Options{}); m.maxHeld != runMaxHeld || m.maxHeldTotal != runsMaxHeld {
		t.Errorf("defaults: %d %d", m.maxHeld, m.maxHeldTotal)
	}
}

// A run refused before it queues leaves nothing counted.
func TestARefusedRunLeavesNothingCounted(t *testing.T) {
	ts, mgr := newRunHarness(t)
	c := runClient{t: t, base: ts.URL}
	code, m := c.do("POST", "/_localbase/runs/v1/chat/completions", `{"messages":[]}`)
	if code != 400 || m["error"] == nil {
		t.Fatalf("not refused: %d %v", code, m)
	}
	if got := mgr.held.Load(); got != 0 {
		t.Errorf("holding %d after a refusal", got)
	}
}

// Runs dropped for being over an owner's retained count give back what they held.
func TestATrimmedRunGivesBackWhatItHeld(t *testing.T) {
	_, mgr := newRunHarness(t)
	m := mgr
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
	dir := t.TempDir()
	first := NewManager(stubOptions(dir))
	r := &run{m: first, notify: make(chan struct{}), done: make(chan struct{})}
	later := time.Now().Add(time.Hour)
	id := newRunID()
	r.Owner = "me"
	r.ID, r.Object, r.Target, r.Status, r.ExpiresAt = id, "localbase.run", chatTarget, runCompleted, &later
	r.output.Content = strings.Repeat("x", 5000)
	first.checkpoint(r)
	second := NewManager(stubOptions(dir))
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

// The refusal says what holds the memory, how much, when the oldest goes, and what to do.
func TestAFullTotalSaysWhatHoldsItAndWhatToDo(t *testing.T) {
	ts, mgr := newRunHarness(t)
	c := runClient{t: t, base: ts.URL}
	first := c.start("hello")
	c.waitStatus(first, "completed")
	done := c.get(first)
	mgr.maxHeldTotal = mgr.held.Load() + 1 // Full, with a finished run holding it.
	m := c.waitStatus(c.start("fake:slow 200"), "failed")
	if m["reason"] != "memory_limit" {
		t.Fatalf("%v", m)
	}
	e, _ := m["error"].(map[string]any)
	if e == nil || e["code"] != CodeMemoryFull {
		t.Fatalf("error: %v", e)
	}
	expires, err := time.Parse(time.RFC3339Nano, done["expiresAt"].(string))
	if err != nil {
		t.Fatal(err)
	}
	message, _ := e["message"].(string)
	for _, want := range []string{"Results are holding 1 MiB", "the limit is 1 MiB", "Finished results hold 1 MiB", "Remove finished results, or wait", "when the last one is removed",
		"expires at " + expires.UTC().Format("15:04") + " UTC"} {
		if !strings.Contains(message, want) {
			t.Errorf("%q lacks %q", message, want)
		}
	}
	if e["finishedBytes"].(float64) <= 0 || e["oldestExpiresAt"] != expires.UTC().Format(time.RFC3339) {
		t.Errorf("fields: %v", e)
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
	outcome, source := relaySSE(context.Background(), w, &eventReader{event: event, left: n}, "m", &inference{}, nil)
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

// The time given is the oldest finished result's, and only finished runs are summed.
func TestAFullTotalNamesTheOldestFinishedResult(t *testing.T) {
	_, mgr := newRunHarness(t)
	m := mgr
	soon, later := time.Date(2026, 11, 3, 9, 15, 0, 0, time.UTC), time.Date(2026, 11, 3, 9, 45, 0, 0, time.UTC)
	m.mu.Lock()
	for i, exp := range []time.Time{later, soon, later} {
		r := &run{m: m, notify: make(chan struct{}), done: make(chan struct{}), held: 1 << 20}
		r.ID, r.Status, r.ExpiresAt = "run_"+strconv.Itoa(i), runCompleted, &exp
		m.runs[r.ID] = r
	}
	active := &run{m: m, notify: make(chan struct{}), done: make(chan struct{}), held: 5 << 20}
	active.ID, active.Status = "run_active", runInProgress
	m.runs[active.ID] = active
	m.mu.Unlock()
	m.held.Store(8 << 20)
	e := m.memoryFullError()
	message := e["message"].(string)
	for _, want := range []string{"Results are holding 8 MiB", "Finished results hold 3 MiB", "expires at 09:15 UTC"} {
		if !strings.Contains(message, want) {
			t.Errorf("%q lacks %q", message, want)
		}
	}
	if e["finishedBytes"] != int64(3<<20) || e["oldestExpiresAt"] != soon.Format(time.RFC3339) {
		t.Errorf("%v", e)
	}
}
