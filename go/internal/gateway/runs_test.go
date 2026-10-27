package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a log sink safe for the gateway's goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitFor polls cond for up to d.
func waitFor(d time.Duration, cond func() bool) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

// Today's contract, kept (DESIGN §21): a client that goes away mid-stream
// cancels its /v1/chat/completions generation, upstream included.
func TestChatDisconnectCancelsUpstream(t *testing.T) {
	cancelLog := filepath.Join(t.TempDir(), "cancel.log")
	t.Setenv("FAKE_CANCEL_LOG", cancelLog)
	logs := &syncBuffer{}
	testLog = logs
	ts := newTestServer(t)
	testLog = nil

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"`+chatModel+`","stream":true,"messages":[{"role":"user","content":"fake:slow 200"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(resp.Body).ReadString('\n')
	if !strings.HasPrefix(line, "data: ") {
		t.Fatalf("first line %q", line)
	}
	cancel() // the browser tab goes away
	resp.Body.Close()

	if !waitFor(5*time.Second, func() bool { b, _ := os.ReadFile(cancelLog); return strings.Contains(string(b), "cancelled") }) {
		t.Fatal("the backend kept generating after the client went away")
	}
	if !waitFor(5*time.Second, func() bool {
		return strings.Contains(logs.String(), `"terminal_source":"request_aborted"`)
	}) {
		t.Errorf("no cancelled inference.completed in the log:\n%s", logs.String())
	}
}

// --- detached runs (DESIGN §21) -------------------------------------------------------

type runEv struct {
	id         int
	name, data string
}

// runClient talks to one gateway as one caller (key "" under --no-auth).
type runClient struct {
	t    *testing.T
	base string
	key  string
}

func (c runClient) do(method, path, body string, hdr ...string) (int, map[string]any) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.base+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func chatBody(content string) string {
	return `{"model":"` + chatModel + `","messages":[{"role":"user","content":"` + content + `"}]}`
}

// start creates a chat run and returns its id.
func (c runClient) start(content string) string {
	c.t.Helper()
	code, m := c.do("POST", "/_localbase/runs/v1/chat/completions", chatBody(content))
	if code != 202 || m["object"] != "localbase.run" {
		c.t.Fatalf("create: %d %v", code, m)
	}
	return m["id"].(string)
}

func (c runClient) get(id string) map[string]any {
	c.t.Helper()
	code, m := c.do("GET", "/_localbase/runs/"+id, "")
	if code != 200 {
		c.t.Fatalf("get %s: %d %v", id, code, m)
	}
	return m
}

// waitStatus polls until the run reaches one of the statuses.
func (c runClient) waitStatus(id string, statuses ...string) map[string]any {
	c.t.Helper()
	var m map[string]any
	if !waitFor(10*time.Second, func() bool {
		m = c.get(id)
		for _, s := range statuses {
			if m["status"] == s {
				return true
			}
		}
		return false
	}) {
		c.t.Fatalf("run %s: status %v, want %v", id, m["status"], statuses)
	}
	return m
}

func content(m map[string]any) string {
	out, _ := m["output"].(map[string]any)
	s, _ := out["content"].(string)
	return s
}

// events reads a run's event stream until stop returns true or the stream
// ends; it returns the events, comments seen, and the response headers.
func (c runClient) events(id string, hdr []string, query string, stop func([]runEv) bool) ([]runEv, int, http.Header) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", c.base+"/_localbase/runs/"+id+"/events"+query, nil)
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		c.t.Fatalf("events %s: %d", id, resp.StatusCode)
	}
	rd := bufio.NewReader(resp.Body)
	var evs []runEv
	comments := 0
	cur := runEv{}
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return evs, comments, resp.Header
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "":
			if cur.name != "" {
				evs = append(evs, cur)
				if stop != nil && stop(evs) {
					return evs, comments, resp.Header
				}
			}
			cur = runEv{}
		case strings.HasPrefix(line, ":"):
			comments++
			if stop != nil && stop(nil) {
				return evs, comments, resp.Header
			}
		case strings.HasPrefix(line, "id: "):
			cur.id, _ = strconv.Atoi(strings.TrimPrefix(line, "id: "))
		case strings.HasPrefix(line, "event: "):
			cur.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		}
	}
}

func chunkText(evs []runEv) string {
	var b strings.Builder
	for _, e := range evs {
		if e.name != "chunk" {
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct{ Content string }
			}
		}
		json.Unmarshal([]byte(e.data), &c)
		for _, ch := range c.Choices {
			b.WriteString(ch.Delta.Content)
		}
	}
	return b.String()
}

func countChunks(evs []runEv) int {
	n := 0
	for _, e := range evs {
		if e.name == "chunk" {
			n++
		}
	}
	return n
}

func words(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "w%d ", i+1)
	}
	return b.String()
}

func runServer(ts *httptest.Server) *Server { return ts.Config.Handler.(*Server) }

// The creating request and every reader go away; the run still finishes
// upstream, and the model is released like after any request.
func TestRunSurvivesDisconnect(t *testing.T) {
	cancelLog := filepath.Join(t.TempDir(), "cancel.log")
	t.Setenv("FAKE_CANCEL_LOG", cancelLog)
	ts, llm, _ := newTestGateway(t, host{})
	c := runClient{t: t, base: ts.URL}
	id := c.start("fake:slow 30")
	evs, _, _ := c.events(id, nil, "", func(e []runEv) bool { return countChunks(e) >= 2 }) // then the tab goes away
	if countChunks(evs) < 2 {
		t.Fatalf("events: %v", evs)
	}
	m := c.waitStatus(id, "completed")
	if content(m) != words(30) {
		t.Errorf("output %q", content(m))
	}
	if b, _ := os.ReadFile(cancelLog); len(b) > 0 {
		t.Errorf("the backend saw a cancel: %s", b)
	}
	if snap := llm.Snapshot(); snap.Active != 0 || snap.Waiting != 0 {
		t.Errorf("a finished run still holds the model: %+v", snap)
	}
	if id, _, _ := llm.Running(); id != chatModel {
		t.Errorf("the model was unloaded: %q", id)
	}
	if m["requestId"] == "" || m["resumable"] != false {
		t.Errorf("run object: %v", m)
	}
}

// A returning page replays exactly what it missed, then follows live.
func TestRunEventsResume(t *testing.T) {
	ts := newTestServer(t)
	c := runClient{t: t, base: ts.URL}
	id := c.start("fake:slow 20")
	first, _, _ := c.events(id, nil, "", func(e []runEv) bool { return countChunks(e) >= 5 })
	last := first[len(first)-1].id
	rest, _, _ := c.events(id, []string{"Last-Event-ID", strconv.Itoa(last)}, "", nil)
	for i, e := range rest {
		if e.id != last+1+i {
			t.Fatalf("ids after %d: %v", last, rest)
		}
	}
	if rest[len(rest)-1].name != "end" {
		t.Errorf("no end event: %v", rest)
	}
	if got := chunkText(first) + chunkText(rest); got != words(20) {
		t.Errorf("first+rest = %q", got)
	}
	// ?after= replays the same way; after=0 is everything.
	all, _, _ := c.events(id, nil, "?after=0", nil)
	if len(all) != last+len(rest) || all[0].id != 1 || chunkText(all) != words(20) {
		t.Errorf("after=0: %d events, %q", len(all), chunkText(all))
	}
	tail, _, _ := c.events(id, nil, "?after="+strconv.Itoa(last), nil)
	if len(tail) != len(rest) {
		t.Errorf("after=%d: %d events, want %d", last, len(tail), len(rest))
	}
}

// Stop cancels upstream and keeps the partial output; dismiss deletes it.
// A queued run holds no admission and stops at once.
func TestRunCancel(t *testing.T) {
	cancelLog := filepath.Join(t.TempDir(), "cancel.log")
	t.Setenv("FAKE_CANCEL_LOG", cancelLog)
	ts, llm, _ := newTestGateway(t, host{})
	c := runClient{t: t, base: ts.URL}
	id := c.start("fake:slow 200")
	c.events(id, nil, "", func(e []runEv) bool { return countChunks(e) >= 3 })

	queued := c.start("fake:slow 5")
	if m := c.get(queued); m["status"] != "queued" || m["startedAt"] != nil {
		t.Errorf("second run: %v", m)
	}
	if snap := llm.Snapshot(); snap.Active != 1 || snap.Waiting != 1 {
		t.Errorf("snapshot with one running and one queued: %+v", snap)
	}
	began := time.Now()
	code, m := c.do("POST", "/_localbase/runs/"+queued+"/cancel", "")
	if code != 200 || m["status"] != "cancelled" || m["reason"] != "stopped" || time.Since(began) > time.Second {
		t.Errorf("stop queued: %d %v after %v", code, m, time.Since(began))
	}

	code, m = c.do("POST", "/_localbase/runs/"+id+"/cancel", "")
	if code != 200 || m["status"] != "cancelled" || m["reason"] != "stopped" || !strings.HasPrefix(content(m), "w1 w2 w3 ") {
		t.Fatalf("stop: %d %v", code, m)
	}
	if !waitFor(5*time.Second, func() bool { b, _ := os.ReadFile(cancelLog); return strings.Contains(string(b), "cancelled") }) {
		t.Error("the backend kept generating after Stop")
	}
	if c.get(id)["status"] != "cancelled" {
		t.Error("the stopped run is gone before dismiss")
	}
	file := filepath.Join(runServer(ts).Root.Dir, "runs", id+".json")
	if _, err := os.Stat(file); err != nil {
		t.Errorf("checkpoint: %v", err)
	}
	if code, _ := c.do("DELETE", "/_localbase/runs/"+id, ""); code != 200 {
		t.Errorf("dismiss: %d", code)
	}
	if code, m := c.do("GET", "/_localbase/runs/"+id, ""); code != 404 || m["error"].(map[string]any)["code"] != "run_not_found" {
		t.Errorf("after dismiss: %d %v", code, m)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("the checkpoint outlived dismiss: %v", err)
	}
}

// authGateway is the test gateway with authentication on and two chat keys
// plus a key without inference:chat.
func authGateway(t *testing.T) (*httptest.Server, string, string, string) {
	t.Helper()
	ts := newTestServer(t)
	s := runServer(ts)
	s.NoAuth = false
	ctx := context.Background()
	_, a, _ := s.Store.CreateAPIKey(ctx, "a", []string{"inference:chat"})
	_, b, _ := s.Store.CreateAPIKey(ctx, "b", []string{"inference:chat"})
	_, none, _ := s.Store.CreateAPIKey(ctx, "none", []string{"models:read"})
	return ts, a, b, none
}

// Runs are owner-bound: another principal sees no run at all.
func TestRunOwnership(t *testing.T) {
	ts, ka, kb, knone := authGateway(t)
	a, b := runClient{t: t, base: ts.URL, key: ka}, runClient{t: t, base: ts.URL, key: kb}
	id := a.start("fake:slow 40")
	for _, req := range [][2]string{{"GET", ""}, {"GET", "/events"}, {"POST", "/cancel"}, {"DELETE", ""}} {
		if code, m := b.do(req[0], "/_localbase/runs/"+id+req[1], ""); code != 404 || m["error"].(map[string]any)["code"] != "run_not_found" {
			t.Errorf("%s %s as another key: %d %v", req[0], req[1], code, m)
		}
	}
	if _, m := b.do("GET", "/_localbase/runs", ""); len(m["data"].([]any)) != 0 {
		t.Errorf("another key's list: %v", m)
	}
	if _, m := a.do("GET", "/_localbase/runs", ""); len(m["data"].([]any)) != 1 {
		t.Errorf("owner's list: %v", m)
	}
	if code, _ := (runClient{t: t, base: ts.URL}).do("GET", "/_localbase/runs/"+id, ""); code != 401 {
		t.Errorf("no credentials: %d", code)
	}
	// Creating needs the target's scope, answered synchronously as /v1 does.
	if code, m := (runClient{t: t, base: ts.URL, key: knone}).do("POST", "/_localbase/runs/v1/chat/completions", chatBody("hi")); code != 403 {
		t.Errorf("no inference:chat: %d %v", code, m)
	}
	if a.waitStatus(id, "completed")["status"] != "completed" {
		t.Error("the owner's run did not complete")
	}
}

// A request /v1 refuses before the queue is refused the same way, and no
// run exists.
func TestRunValidationSynchronous(t *testing.T) {
	ts := newTestServer(t)
	c := runClient{t: t, base: ts.URL}
	for body, want := range map[string]int{`{"messages":[]}`: 400, `{"model":"nope","messages":[{"role":"user","content":"x"}]}`: 404, `not json`: 400} {
		v1 := post(t, ts, "/v1/chat/completions", body)
		code, m := c.do("POST", "/_localbase/runs/v1/chat/completions", body)
		if code != want || code != v1.status || m["error"] == nil {
			t.Errorf("%s: run %d %v, /v1 %d", body, code, m, v1.status)
		}
	}
	if code, _ := c.do("POST", "/_localbase/runs/v1/embeddings", `{}`); code != 404 {
		t.Errorf("a target runs do not wrap: %d", code)
	}
	if _, m := c.do("GET", "/_localbase/runs", ""); len(m["data"].([]any)) != 0 {
		t.Errorf("refused requests left runs: %v", m)
	}
}

// A restart (graceful, or a crash found at the next start) reports the run
// interrupted with the checkpointed partial text; continuing is the page's.
func TestRunRestartInterrupted(t *testing.T) {
	ts := newTestServer(t)
	a := runServer(ts)
	c := runClient{t: t, base: ts.URL}
	id := c.start("fake:slow 200")
	c.events(id, nil, "", func(e []runEv) bool { return countChunks(e) >= 25 }) // > 1 s: a checkpoint holds output

	// A crash: a new gateway starts on the root while the old record says
	// in_progress.
	b := New(&Server{Store: a.Store, Catalog: a.Catalog, Auth: a.Auth, LLM: a.LLM, Log: a.Log, NoAuth: true, MemoryGB: 64,
		Root: a.Root, Memory: a.Memory})
	tb := httptest.NewServer(b)
	defer tb.Close()
	cb := runClient{t: t, base: tb.URL}
	m := cb.get(id)
	cont, _ := m["continuation"].(map[string]any)
	if m["status"] != "interrupted" || m["reason"] != "gateway_restart" || !strings.HasPrefix(content(m), "w1 w2 w3 ") ||
		m["resumable"] != false || cont["by"] != "client" {
		t.Fatalf("after a crash: %v", m)
	}
	evs, _, _ := cb.events(id, nil, "", nil)
	if len(evs) != 2 || evs[0].name != "snapshot" || evs[1].name != "end" || !strings.Contains(evs[0].data, `"content":"w1 w2`) {
		t.Errorf("events after restart: %v", evs)
	}
	// A page resuming from before the restart (its last id may be past the
	// last checkpoint) still learns the outcome: the buffer is gone, so the
	// snapshot and end come whatever id it sends, numbered after the
	// checkpointed events (FE thread, 2026-10-27).
	for _, resume := range []struct {
		hdr   []string
		query string
	}{{[]string{"Last-Event-ID", "52"}, ""}, {nil, "?after=100000"}, {[]string{"Last-Event-ID", "1"}, ""}} {
		got, _, _ := cb.events(id, resume.hdr, resume.query, nil)
		if len(got) != 2 || got[0].name != "snapshot" || got[1].name != "end" || got[0].id != evs[0].id || got[1].id != evs[1].id || evs[0].id < 2 {
			t.Errorf("resume %v%s after restart: %v", resume.hdr, resume.query, got)
		}
	}
	if last := m["lastEventId"].(float64); last != float64(evs[1].id) {
		t.Errorf("lastEventId %v, end id %d", last, evs[1].id)
	}

	// A graceful stop interrupts the live run.
	a.StopRuns(5 * time.Second)
	m = c.get(id)
	if m["status"] != "interrupted" || m["reason"] != "gateway_restart" || content(m) == "" {
		t.Errorf("after a graceful stop: %v", m)
	}
	if code, _ := c.do("POST", "/_localbase/runs/v1/chat/completions", chatBody("hi")); code != 503 {
		t.Errorf("create while stopping: %d", code)
	}
	b.StopRuns(time.Second)
}

// A finished run is deleted with its checkpoint after its retention.
func TestRunExpiry(t *testing.T) {
	logs := &syncBuffer{}
	testLog = logs
	ts := newTestServer(t)
	testLog = nil
	s := runServer(ts)
	c := runClient{t: t, base: ts.URL}
	id := c.start("fake:reply done")
	c.waitStatus(id, "completed")
	dir := filepath.Join(s.Root.Dir, "runs")
	fi, err := os.Stat(filepath.Join(dir, id+".json"))
	di, _ := os.Stat(dir)
	if err != nil || fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Fatalf("checkpoint modes: %v %v %v", err, fi, di)
	}
	s.runs.now = func() time.Time { return time.Now().Add(31 * time.Minute) }
	if _, m := c.do("GET", "/_localbase/runs", ""); len(m["data"].([]any)) != 0 {
		t.Errorf("expired run listed: %v", m)
	}
	if code, _ := c.do("GET", "/_localbase/runs/"+id, ""); code != 404 {
		t.Errorf("expired run: %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, id+".json")); !os.IsNotExist(err) {
		t.Errorf("expired checkpoint: %v", err)
	}
	if !strings.Contains(logs.String(), `"event":"run.expired"`) {
		t.Error("no run.expired event")
	}
	// The startup sweep removes expired files too.
	os.WriteFile(filepath.Join(dir, "run_00.json"), []byte(`{"id":"run_00","owner":"local","status":"completed","expiresAt":"2020-01-01T00:00:00Z"}`), 0o600)
	os.WriteFile(filepath.Join(dir, "junk.json"), []byte(`x`), 0o600)
	New(&Server{Store: s.Store, Catalog: s.Catalog, Auth: s.Auth, LLM: s.LLM, Log: s.Log, NoAuth: true, Root: s.Root, Memory: s.Memory}).StopRuns(0)
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("left after the startup sweep: %v", left)
	}
}

// Per-principal active runs and buffered output are capped.
func TestRunLimits(t *testing.T) {
	cancelLog := filepath.Join(t.TempDir(), "cancel.log")
	t.Setenv("FAKE_CANCEL_LOG", cancelLog)
	ts := newTestServer(t)
	s := runServer(ts)
	c := runClient{t: t, base: ts.URL}
	var ids []string
	for range runActivePerOwner {
		ids = append(ids, c.start("fake:slow 200"))
	}
	code, m := c.do("POST", "/_localbase/runs/v1/chat/completions", chatBody("hi"))
	if code != 429 || m["error"].(map[string]any)["code"] != "too_many_runs" {
		t.Errorf("over the active cap: %d %v", code, m)
	}
	for _, id := range ids {
		c.do("POST", "/_localbase/runs/"+id+"/cancel", "")
	}
	os.Remove(cancelLog)
	s.runs.maxOutput = 2000
	id := c.start("fake:slow 200")
	m = c.waitStatus(id, "failed")
	if m["reason"] != "output_limit" || content(m) == "" {
		t.Errorf("over the output cap: %v", m)
	}
	if !waitFor(5*time.Second, func() bool { b, _ := os.ReadFile(cancelLog); return strings.Contains(string(b), "cancelled") }) {
		t.Error("the backend kept generating past the output cap")
	}
}

// A critical-memory stop mid-run is reported as such.
func TestRunMemoryPressure(t *testing.T) {
	ts, llm, critical := emergencyGateway(t)
	c := runClient{t: t, base: ts.URL}
	id := c.start("fake:slow 200")
	waitRunning(t, llm)
	c.events(id, nil, "", func(e []runEv) bool { return countChunks(e) >= 3 })
	critical()
	m := c.waitStatus(id, "interrupted", "failed", "completed")
	if m["status"] != "interrupted" || m["reason"] != "memory_pressure" || content(m) == "" {
		t.Errorf("after a memory emergency: %v", m)
	}
}

// A quiet run keeps its stream alive with comments, and the stream headers
// suit Cloudflare and Safari.
func TestRunKeepAlive(t *testing.T) {
	t.Setenv("FAKE_DELAY_MS", "1500") // nothing to say for 1.5 s
	ts := newTestServer(t)
	runServer(ts).runs.ping = 100 * time.Millisecond
	c := runClient{t: t, base: ts.URL}
	id := c.start("fake:reply quiet")
	_, comments, h := c.events(id, nil, "", func(e []runEv) bool { return e == nil && false })
	if comments < 5 {
		t.Errorf("%d keep-alive comments over a quiet 1.5 s", comments)
	}
	if h.Get("Cache-Control") != "no-store, no-transform" || h.Get("X-Accel-Buffering") != "no" || !strings.HasPrefix(h.Get("Content-Type"), "text/event-stream") {
		t.Errorf("headers: %v", h)
	}
}

// Lifecycle events carry ids, never content.
func TestRunLogs(t *testing.T) {
	logs := &syncBuffer{}
	testLog = logs
	ts := newTestServer(t)
	testLog = nil
	c := runClient{t: t, base: ts.URL}
	id := c.start("fake:reply secretOutputText42")
	m := c.waitStatus(id, "completed")
	if content(m) != "secretOutputText42 " {
		t.Fatalf("output %q", content(m))
	}
	c.do("DELETE", "/_localbase/runs/"+id, "")
	out := logs.String()
	for _, ev := range []string{"run.started", "run.completed", "run.dismissed"} {
		if !strings.Contains(out, `"event":"`+ev+`"`) {
			t.Errorf("no %s", ev)
		}
	}
	if !strings.Contains(out, `"run_id":"`+id+`"`) || !strings.Contains(out, `"request_id":"`+m["requestId"].(string)+`"`) {
		t.Errorf("ids missing:\n%s", out)
	}
	if strings.Contains(out, "secretOutputText42") {
		t.Error("run content reached the log")
	}
}

// The UI reaches runs through /app/api; the event stream keeps `after`.
func TestRunBrowserRoutes(t *testing.T) {
	for _, c := range []struct {
		method, path string
		ok           bool
	}{
		{"GET", "/_localbase/runs", true}, {"POST", "/_localbase/runs", false},
		{"POST", "/_localbase/runs/v1/chat/completions", true}, {"POST", "/_localbase/runs/v1/embeddings", false},
		{"GET", "/_localbase/runs/run_1", true}, {"DELETE", "/_localbase/runs/run_1", true}, {"PATCH", "/_localbase/runs/run_1", false},
		{"GET", "/_localbase/runs/run_1/events", true}, {"POST", "/_localbase/runs/run_1/cancel", true},
		{"GET", "/_localbase/runs/run_1/cancel", false}, {"GET", "/_localbase/runs/run_1/x/y", false},
	} {
		if got := allowedUIRoute(c.method, c.path); got != c.ok {
			t.Errorf("%s %s: %v", c.method, c.path, got)
		}
	}
	ts := newTestServer(t)
	c := runClient{t: t, base: ts.URL}
	code, m := c.do("POST", "/app/api/_localbase/runs/v1/chat/completions", chatBody("fake:slow 5"))
	if code != 202 {
		t.Fatalf("create through /app/api: %d %v", code, m)
	}
	id := m["id"].(string)
	c.waitStatus(id, "completed")
	req, _ := http.NewRequest("GET", ts.URL+"/app/api/_localbase/runs/"+id+"/events?after=3", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(b), "id: 3\n") || !strings.Contains(string(b), "id: 4\n") {
		t.Errorf("after=3 through /app/api:\n%s", b)
	}
}
