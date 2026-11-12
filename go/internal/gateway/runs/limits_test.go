package runs

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
)

// A chat run over what it may hold ends failed, with what it had, and the backend is told to stop.
func TestARunOverItsMemoryLimitFailsWithWhatItHad(t *testing.T) {
	ts, mgr := newRunHarness(t)
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	mgr.maxHeld = 3000
	m := c.WaitStatus(c.Start("fake:slow 200"), "failed")
	if m["reason"] != "output_limit" || gatewaytest.Content(m) == "" {
		t.Errorf("%v", m)
	}
	if got := mgr.held.Load(); got > 3000 {
		t.Errorf("holding %d, limit 3000", got)
	}
}

// All runs together have a limit too, with its own reason, and dropping a run gives its share back.
func TestRunsOverTheirTotalMemoryLimitFailAndGiveItBack(t *testing.T) {
	ts, mgr := newRunHarness(t)
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	mgr.maxHeldTotal = 3000
	id := c.Start("fake:slow 200")
	m := c.WaitStatus(id, "failed")
	if m["reason"] != "memory_limit" || gatewaytest.Content(m) == "" {
		t.Errorf("%v", m)
	}
	if got := mgr.held.Load(); got == 0 || got > 3000 {
		t.Errorf("holding %d, limit 3000", got)
	}
	e, _ := m["error"].(map[string]any)
	if e == nil || e["code"] != CodeMemoryFull || !strings.Contains(e["message"].(string), "wait for the running ones") {
		t.Errorf("nothing else had finished: %v", e)
	}
	if code, _ := c.Do("DELETE", "/_localbase/runs/"+id, ""); code != 200 {
		t.Fatalf("dismissing: %d", code)
	}
	if got := mgr.held.Load(); got != 0 {
		t.Errorf("holding %d after dismissing the only run", got)
	}
}

// The other ways a run goes give it back too.
func TestAnExpiredRunGivesBackWhatItHeld(t *testing.T) {
	ts, mgr := newRunHarness(t)
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	id := c.Start("hello")
	c.WaitStatus(id, "completed")
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
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	code, m := c.Do("POST", "/_localbase/runs/v1/chat/completions", `{"messages":[]}`)
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
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	first := c.Start("hello")
	c.WaitStatus(first, "completed")
	done := c.Get(first)
	mgr.maxHeldTotal = mgr.held.Load() + 1 // Full, with a finished run holding it.
	m := c.WaitStatus(c.Start("fake:slow 200"), "failed")
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
		"and the oldest expires ", " after this was stopped (" + expires.UTC().Format("15:04") + " UTC)."} {
		if !strings.Contains(message, want) {
			t.Errorf("%q lacks %q", message, want)
		}
	}
	if e["finishedBytes"].(float64) <= 0 || e["oldestExpiresAt"] != expires.UTC().Format(time.RFC3339) {
		t.Errorf("fields: %v", e)
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
	m.now = func() time.Time { return soon.Add(-29 * time.Minute) } // stopped 29 minutes before
	e := m.memoryFullError()
	message := e["message"].(string)
	for _, want := range []string{"Results are holding 8 MiB", "Finished results hold 3 MiB",
		"and the oldest expires 29 minutes after this was stopped (09:15 UTC). Remove finished results"} {
		if !strings.Contains(message, want) {
			t.Errorf("%q lacks %q", message, want)
		}
	}
	if e["finishedBytes"] != int64(3<<20) || e["oldestExpiresAt"] != soon.Format(time.RFC3339) {
		t.Errorf("%v", e)
	}
}

// How long until the oldest result goes, from when the run was stopped: a person reads it in their own
// time zone, and a countdown read later would be wrong. Rounded up to whole minutes.
func TestTheWaitIsSaidAsTimeAfterTheStop(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{0, "less than a minute"},
		{30 * time.Second, "less than a minute"},
		{time.Minute, "1 minute"},
		{time.Minute + time.Second, "2 minutes"},
		{29 * time.Minute, "29 minutes"},
		{59*time.Minute + 30*time.Second, "1 hour"},
		{65 * time.Minute, "1 hour 5 minutes"},
		{61 * time.Minute, "1 hour 1 minute"},
		{121 * time.Minute, "2 hours 1 minute"},
		{120 * time.Minute, "2 hours"},
		{-time.Minute, "less than a minute"},
	} {
		if got := afterTheStop(c.d); got != c.want {
			t.Errorf("%v: %q, want %q", c.d, got, c.want)
		}
	}
}
