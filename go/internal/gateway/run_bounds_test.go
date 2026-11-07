package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// Review 02, M3: a run refused before the queue is visible for a moment, and a DELETE in that moment
// waited forever for an end that never came.
func TestADeleteOfARunRefusedBeforeTheQueueReturns(t *testing.T) {
	ts := newTestServer(t)
	s := runServer(ts)
	c := runClient{t: t, base: ts.URL}
	type answer struct {
		code int
		err  error
	}
	deleted := make(chan answer, 1)
	var r *run
	s.runs.afterPublish = func() {
		s.runs.afterPublish = nil
		s.runs.mu.Lock()
		for _, o := range s.runs.runs {
			r = o
		}
		s.runs.mu.Unlock()
		go func() {
			req, _ := http.NewRequest("DELETE", ts.URL+"/_localbase/runs/"+r.ID, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				deleted <- answer{0, err}
				return
			}
			resp.Body.Close()
			deleted <- answer{resp.StatusCode, nil}
		}()
		// The DELETE has found the run and stopped it, and now waits for it to end.
		waitFor(5*time.Second, func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.stopStatus != "" })
	}
	code, m := c.do("POST", "/_localbase/runs/v1/chat/completions", `{"model":"nope","messages":[{"role":"user","content":"x"}]}`)
	if code != 404 || m["error"] == nil {
		t.Errorf("the refusal: %d %v", code, m)
	}
	select {
	case a := <-deleted:
		if a.err != nil || a.code != 200 {
			t.Errorf("the DELETE: %d %v", a.code, a.err)
		}
	case <-time.After(15 * time.Second):
		close(r.done) // so the test can end
		t.Fatal("a DELETE of a run refused before the queue was still waiting after 15 s")
	}
	if _, m := c.do("GET", "/_localbase/runs", ""); len(m["data"].([]any)) != 0 {
		t.Errorf("runs left: %v", m)
	}
}

// readTracker counts what was read of a request body.
type readTracker struct {
	r    io.Reader
	read int
}

func (t *readTracker) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	t.read += n
	return n, err
}

// Review 02, M7: the limit on active runs is checked before a body is read, not after. A media body
// is read whole (up to runMaxBody) before the route sees it; a refusal drains only a bounded part.
func TestTheActiveRunLimitComesBeforeTheBody(t *testing.T) {
	ts := newTestServer(t)
	s := runServer(ts)
	c := runClient{t: t, base: ts.URL}
	var ids []string
	for range runActivePerOwner {
		ids = append(ids, c.start("fake:slow 200"))
	}
	defer func() {
		for _, id := range ids {
			c.do("POST", "/_localbase/runs/"+id+"/cancel", "")
		}
	}()
	const size = 8 << 20 // over the drain of a refused body, under runMaxBody
	body := &readTracker{r: io.LimitReader(zeros{}, size)}
	req := httptest.NewRequest("POST", "/_localbase/runs/v1/images/generations", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.runs.create(rec, req, "/v1/images/generations")
	if rec.Code != 429 || !strings.Contains(rec.Body.String(), "too_many_runs") {
		t.Errorf("over the active limit: %d %s", rec.Code, rec.Body)
	}
	if limit := int(DefaultLimits.RefusedDrainBytes) + 1; body.read > limit {
		t.Errorf("read %d bytes of a body it was going to refuse; a refusal drains at most %d", body.read, limit)
	}
}

// zeros is an endless body.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// Review 02, M7: a run's body counts towards the runs' memory while its request holds it. A body that
// doesn't fit gives a run that has already ended, as a run stopped by that limit ends; a later one
// that fits still runs, and the body is given back when its request ends.
func TestARunsBodyCountsTowardsTheRunsMemory(t *testing.T) {
	ts := newTestServer(t)
	s := runServer(ts)
	c := runClient{t: t, base: ts.URL}
	s.runs.maxHeldTotal = 8000

	big := chatBody(words(2000))
	code, m := c.do("POST", "/_localbase/runs/v1/chat/completions", big)
	if code != 202 || m["status"] != "failed" || m["reason"] != "memory_limit" || content(m) != "" {
		t.Fatalf("a body over the limit: %d %v", code, m)
	}
	e, _ := m["error"].(map[string]any)
	if e == nil || e["code"] != CodeRunsMemoryFull || e["type"] != "server_error" ||
		!strings.Contains(e["message"].(string), "Results are holding") {
		t.Errorf("its error: %v", e)
	}
	if got := c.get(m["id"].(string)); got["status"] != "failed" || got["reason"] != "memory_limit" {
		t.Errorf("read back: %v", got)
	}
	if got := s.runs.held.Load(); got != 0 {
		t.Errorf("holding %d after a body that was never kept", got)
	}

	small := chatBody("fake:slow 3")
	var atPublish int64
	s.runs.afterPublish = func() { atPublish = s.runs.held.Load() }
	code, m = c.do("POST", "/_localbase/runs/v1/chat/completions", small)
	if code != 202 {
		t.Fatalf("a body that fits: %d %v", code, m)
	}
	id := m["id"].(string)
	if atPublish < int64(len(small)) {
		t.Errorf("holding %d while the request holds a body of %d", atPublish, len(small))
	}
	if m = c.waitStatus(id, "completed"); content(m) != words(3) {
		t.Errorf("output %q", content(m))
	}
	s.runs.mu.Lock()
	r := s.runs.runs[id]
	s.runs.mu.Unlock()
	r.mu.Lock()
	output := r.held
	r.mu.Unlock()
	if got := s.runs.held.Load(); got != output {
		t.Errorf("holding %d once the run ended, and its output holds %d: the body was not given back", got, output)
	}
}

// stuckRun is a run whose inner request has not ended and won't obey a stop, holding output and a
// body.
func stuckRun(m *runManager) *run {
	r := &run{m: m, notify: make(chan struct{}), done: make(chan struct{}), cancel: func() {}, queuedSeen: true}
	r.ID, r.Object, r.Owner, r.Target, r.Status = newRunID(), "localbase.run", "local", chatTarget, runInProgress
	r.held, r.body = 1000, 500
	m.held.Add(1500)
	m.mu.Lock()
	m.runs[r.ID] = r
	m.mu.Unlock()
	return r
}

// A DELETE waits for a stopped run as long as a cancel does, then dismisses it anyway. What the run
// still held is given back then, and what it took after is given back when it ends.
func TestADeleteOfARunThatWontStopIsBounded(t *testing.T) {
	ts := newTestServer(t)
	s := runServer(ts)
	m := s.runs
	m.stopWait = 50 * time.Millisecond
	r := stuckRun(m)
	c := runClient{t: t, base: ts.URL}
	if code, body := c.do("DELETE", "/_localbase/runs/"+r.ID, ""); code != 200 || body["deleted"] != true {
		t.Fatalf("the DELETE: %d %v", code, body)
	}
	if got := m.held.Load(); got != 0 {
		t.Errorf("holding %d after the DELETE", got)
	}
	r.mu.Lock() // it took a little more before it ended
	r.held += 300
	m.held.Add(300)
	r.mu.Unlock()
	m.finish(r, &runWriter{r: r, header: http.Header{}, status: 200, sse: true})
	if got := m.held.Load(); got != 0 {
		t.Errorf("holding %d after the dismissed run ended", got)
	}
	if m.dir != "" {
		if _, err := os.Stat(m.path(r.ID)); !os.IsNotExist(err) {
			t.Errorf("the dismissed run's checkpoint is back: %v", err)
		}
	}
	if _, list := c.do("GET", "/_localbase/runs", ""); len(list["data"].([]any)) != 0 {
		t.Errorf("runs left: %v", list)
	}
}

// A DELETE whose caller leaves stops waiting, and leaves the run, stopped, to expire.
func TestADeleteWhoseCallerLeavesStopsWaiting(t *testing.T) {
	ts := newTestServer(t)
	s := runServer(ts)
	m := s.runs
	m.stopWait = time.Hour
	r := stuckRun(m)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequestWithContext(ctx, "DELETE", "/_localbase/runs/"+r.ID, nil)
	req.SetPathValue("rest", r.ID)
	rec := httptest.NewRecorder()
	returned := make(chan struct{})
	go func() { s.runRoute(rec, req); close(returned) }()
	time.AfterFunc(50*time.Millisecond, cancel)
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		close(r.done)
		t.Fatal("a DELETE whose caller left was still waiting after 5 s")
	}
	m.mu.Lock()
	_, kept := m.runs[r.ID]
	m.mu.Unlock()
	r.mu.Lock()
	stopped := r.stopStatus == runCancelled
	r.mu.Unlock()
	if !kept || !stopped || rec.Body.Len() != 0 {
		t.Errorf("kept %v, stopped %v, answered %q", kept, stopped, rec.Body)
	}
}
