package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
)

// Today's contract, kept (DESIGN §21): a client that goes away mid-stream
// cancels its /v1/chat/completions generation, upstream included.
func TestChatDisconnectCancelsUpstream(t *testing.T) {
	cancelLog := filepath.Join(t.TempDir(), "cancel.log")
	t.Setenv("FAKE_CANCEL_LOG", cancelLog)
	logs := &gatewaytest.SyncBuffer{}
	gatewaytest.Log = logs
	ts := newTestServer(t)
	gatewaytest.Log = nil

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"`+gatewaytest.ChatModel+`","stream":true,"messages":[{"role":"user","content":"fake:slow 200"}]}`))
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

	if !gatewaytest.WaitFor(5*time.Second, func() bool { b, _ := os.ReadFile(cancelLog); return strings.Contains(string(b), "cancelled") }) {
		t.Fatal("the backend kept generating after the client went away")
	}
	if !gatewaytest.WaitFor(5*time.Second, func() bool {
		return strings.Contains(logs.String(), `"terminal_source":"request_aborted"`)
	}) {
		t.Errorf("no cancelled inference.completed in the log:\n%s", logs.String())
	}
}

// --- detached runs (DESIGN §21) -------------------------------------------------------

func chunkText(evs []gatewaytest.RunEvent) string {
	var b strings.Builder
	for _, e := range evs {
		if e.Name != "chunk" {
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct{ Content string }
			}
		}
		json.Unmarshal([]byte(e.Data), &c)
		for _, ch := range c.Choices {
			b.WriteString(ch.Delta.Content)
		}
	}
	return b.String()
}

func countChunks(evs []gatewaytest.RunEvent) int {
	n := 0
	for _, e := range evs {
		if e.Name == "chunk" {
			n++
		}
	}
	return n
}

func runServer(ts *httptest.Server) *Server { return ts.Config.Handler.(*Server) }

// The creating request and every reader go away; the run still finishes
// upstream, and the model is released like after any request.
func TestRunSurvivesDisconnect(t *testing.T) {
	cancelLog := filepath.Join(t.TempDir(), "cancel.log")
	t.Setenv("FAKE_CANCEL_LOG", cancelLog)
	ts, llm, _ := newTestGateway(t, host{})
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	id := c.Start("fake:slow 30")
	evs, _, _ := c.Events(id, nil, "", func(e []gatewaytest.RunEvent) bool { return countChunks(e) >= 2 }) // then the tab goes away
	if countChunks(evs) < 2 {
		t.Fatalf("events: %v", evs)
	}
	m := c.WaitStatus(id, "completed")
	if gatewaytest.Content(m) != gatewaytest.Words(30) {
		t.Errorf("output %q", gatewaytest.Content(m))
	}
	if b, _ := os.ReadFile(cancelLog); len(b) > 0 {
		t.Errorf("the backend saw a cancel: %s", b)
	}
	if snap := llm.Snapshot(); snap.Active != 0 || snap.Waiting != 0 {
		t.Errorf("a finished run still holds the model: %+v", snap)
	}
	if id, _, _ := llm.Running(); id != gatewaytest.ChatModel {
		t.Errorf("the model was unloaded: %q", id)
	}
	if m["requestId"] == "" || m["resumable"] != false {
		t.Errorf("run object: %v", m)
	}
}

// A returning page replays exactly what it missed, then follows live.
func TestRunEventsResume(t *testing.T) {
	ts := newTestServer(t)
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	id := c.Start("fake:slow 20")
	first, _, _ := c.Events(id, nil, "", func(e []gatewaytest.RunEvent) bool { return countChunks(e) >= 5 })
	last := first[len(first)-1].ID
	rest, _, _ := c.Events(id, []string{"Last-Event-ID", strconv.Itoa(last)}, "", nil)
	for i, e := range rest {
		if e.ID != last+1+i {
			t.Fatalf("ids after %d: %v", last, rest)
		}
	}
	if rest[len(rest)-1].Name != "end" {
		t.Errorf("no end event: %v", rest)
	}
	if got := chunkText(first) + chunkText(rest); got != gatewaytest.Words(20) {
		t.Errorf("first+rest = %q", got)
	}
	// ?after= replays the same way; after=0 is everything.
	all, _, _ := c.Events(id, nil, "?after=0", nil)
	if len(all) != last+len(rest) || all[0].ID != 1 || chunkText(all) != gatewaytest.Words(20) {
		t.Errorf("after=0: %d events, %q", len(all), chunkText(all))
	}
	tail, _, _ := c.Events(id, nil, "?after="+strconv.Itoa(last), nil)
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
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	id := c.Start("fake:slow 200")
	c.Events(id, nil, "", func(e []gatewaytest.RunEvent) bool { return countChunks(e) >= 3 })

	queued := c.Start("fake:slow 5")
	if m := c.Get(queued); m["status"] != "queued" || m["startedAt"] != nil {
		t.Errorf("second run: %v", m)
	}
	// A run reports queued just before it joins the queue, so wait for the count.
	if !gatewaytest.WaitFor(5*time.Second, func() bool { snap := llm.Snapshot(); return snap.Active == 1 && snap.Waiting == 1 }) {
		t.Errorf("snapshot with one running and one queued: %+v", llm.Snapshot())
	}
	began := time.Now()
	code, m := c.Do("POST", "/_localbase/runs/"+queued+"/cancel", "")
	if code != 200 || m["status"] != "cancelled" || m["reason"] != "stopped" || time.Since(began) > time.Second {
		t.Errorf("stop queued: %d %v after %v", code, m, time.Since(began))
	}

	code, m = c.Do("POST", "/_localbase/runs/"+id+"/cancel", "")
	if code != 200 || m["status"] != "cancelled" || m["reason"] != "stopped" || !strings.HasPrefix(gatewaytest.Content(m), "w1 w2 w3 ") {
		t.Fatalf("stop: %d %v", code, m)
	}
	if !gatewaytest.WaitFor(5*time.Second, func() bool { b, _ := os.ReadFile(cancelLog); return strings.Contains(string(b), "cancelled") }) {
		t.Error("the backend kept generating after Stop")
	}
	if c.Get(id)["status"] != "cancelled" {
		t.Error("the stopped run is gone before dismiss")
	}
	file := filepath.Join(runServer(ts).cfg.Root.Dir, "runs", id+".json")
	if _, err := os.Stat(file); err != nil {
		t.Errorf("checkpoint: %v", err)
	}
	if code, _ := c.Do("DELETE", "/_localbase/runs/"+id, ""); code != 200 {
		t.Errorf("dismiss: %d", code)
	}
	if code, m := c.Do("GET", "/_localbase/runs/"+id, ""); code != 404 || m["error"].(map[string]any)["code"] != "run_not_found" {
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
	ts := newTestServerConfigured(t, func(c *Config) { c.NoAuth = false })
	s := runServer(ts)
	ctx := context.Background()
	_, a, _ := s.cfg.Store.CreateAPIKey(ctx, "a", []string{"inference:chat"})
	_, b, _ := s.cfg.Store.CreateAPIKey(ctx, "b", []string{"inference:chat"})
	_, none, _ := s.cfg.Store.CreateAPIKey(ctx, "none", []string{"models:read"})
	return ts, a, b, none
}

// Runs are owner-bound: another principal sees no run at all.
func TestRunOwnership(t *testing.T) {
	ts, ka, kb, knone := authGateway(t)
	a, b := gatewaytest.RunClient{T: t, Base: ts.URL, Key: ka}, gatewaytest.RunClient{T: t, Base: ts.URL, Key: kb}
	id := a.Start("fake:slow 40")
	for _, req := range [][2]string{{"GET", ""}, {"GET", "/events"}, {"POST", "/cancel"}, {"DELETE", ""}} {
		if code, m := b.Do(req[0], "/_localbase/runs/"+id+req[1], ""); code != 404 || m["error"].(map[string]any)["code"] != "run_not_found" {
			t.Errorf("%s %s as another key: %d %v", req[0], req[1], code, m)
		}
	}
	if _, m := b.Do("GET", "/_localbase/runs", ""); len(m["data"].([]any)) != 0 {
		t.Errorf("another key's list: %v", m)
	}
	if _, m := a.Do("GET", "/_localbase/runs", ""); len(m["data"].([]any)) != 1 {
		t.Errorf("owner's list: %v", m)
	}
	if code, _ := (gatewaytest.RunClient{T: t, Base: ts.URL}).Do("GET", "/_localbase/runs/"+id, ""); code != 401 {
		t.Errorf("no credentials: %d", code)
	}
	// Creating needs the target's scope, answered synchronously as /v1 does.
	if code, m := (gatewaytest.RunClient{T: t, Base: ts.URL, Key: knone}).Do("POST", "/_localbase/runs/v1/chat/completions", gatewaytest.ChatBody("hi")); code != 403 {
		t.Errorf("no inference:chat: %d %v", code, m)
	}
	if a.WaitStatus(id, "completed")["status"] != "completed" {
		t.Error("the owner's run did not complete")
	}
}

// A request /v1 refuses before the queue is refused the same way, and no
// run exists.
func TestRunValidationSynchronous(t *testing.T) {
	ts := newTestServer(t)
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	for body, want := range map[string]int{`{"messages":[]}`: 400, `{"model":"nope","messages":[{"role":"user","content":"x"}]}`: 404, `not json`: 400} {
		v1 := post(t, ts, "/v1/chat/completions", body)
		code, m := c.Do("POST", "/_localbase/runs/v1/chat/completions", body)
		if code != want || code != v1.status || m["error"] == nil {
			t.Errorf("%s: run %d %v, /v1 %d", body, code, m, v1.status)
		}
	}
	if code, _ := c.Do("POST", "/_localbase/runs/v1/embeddings", `{}`); code != 404 {
		t.Errorf("a target runs do not wrap: %d", code)
	}
	if _, m := c.Do("GET", "/_localbase/runs", ""); len(m["data"].([]any)) != 0 {
		t.Errorf("refused requests left runs: %v", m)
	}
}

// A restart (graceful, or a crash found at the next start) reports the run
// interrupted with the checkpointed partial text; continuing is the page's.
func TestRunRestartInterrupted(t *testing.T) {
	ts := newTestServer(t)
	a := runServer(ts)
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	id := c.Start("fake:slow 200")
	c.Events(id, nil, "", func(e []gatewaytest.RunEvent) bool { return countChunks(e) >= 25 }) // > 1 s: a checkpoint holds output

	// A crash: a new gateway starts on the root while the old record says
	// in_progress.
	b := newGateway(t, Config{Store: a.cfg.Store, Catalog: a.cfg.Catalog, Auth: a.cfg.Auth, LLM: a.cfg.LLM, Log: a.cfg.Log, NoAuth: true, MemoryGB: 64,
		Root: a.cfg.Root, Memory: a.cfg.Memory})
	tb := httptest.NewServer(b)
	defer tb.Close()
	cb := gatewaytest.RunClient{T: t, Base: tb.URL}
	m := cb.Get(id)
	cont, _ := m["continuation"].(map[string]any)
	if m["status"] != "interrupted" || m["reason"] != "gateway_restart" || !strings.HasPrefix(gatewaytest.Content(m), "w1 w2 w3 ") ||
		m["resumable"] != false || cont["by"] != "client" {
		t.Fatalf("after a crash: %v", m)
	}
	evs, _, _ := cb.Events(id, nil, "", nil)
	if len(evs) != 2 || evs[0].Name != "snapshot" || evs[1].Name != "end" || !strings.Contains(evs[0].Data, `"content":"w1 w2`) {
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
		got, _, _ := cb.Events(id, resume.hdr, resume.query, nil)
		if len(got) != 2 || got[0].Name != "snapshot" || got[1].Name != "end" || got[0].ID != evs[0].ID || got[1].ID != evs[1].ID || evs[0].ID < 2 {
			t.Errorf("resume %v%s after restart: %v", resume.hdr, resume.query, got)
		}
	}
	if last := m["lastEventId"].(float64); last != float64(evs[1].ID) {
		t.Errorf("lastEventId %v, end id %d", last, evs[1].ID)
	}

	// A graceful stop interrupts the live run.
	a.StopRuns(5 * time.Second)
	m = c.Get(id)
	if m["status"] != "interrupted" || m["reason"] != "gateway_restart" || gatewaytest.Content(m) == "" {
		t.Errorf("after a graceful stop: %v", m)
	}
	if code, _ := c.Do("POST", "/_localbase/runs/v1/chat/completions", gatewaytest.ChatBody("hi")); code != 503 {
		t.Errorf("create while stopping: %d", code)
	}
	b.StopRuns(time.Second)
}

// A critical-memory stop mid-run is reported as such.
func TestRunMemoryPressure(t *testing.T) {
	ts, llm, critical := emergencyGateway(t)
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	id := c.Start("fake:slow 200")
	waitRunning(t, llm)
	c.Events(id, nil, "", func(e []gatewaytest.RunEvent) bool { return countChunks(e) >= 3 })
	critical()
	m := c.WaitStatus(id, "interrupted", "failed", "completed")
	if m["status"] != "interrupted" || m["reason"] != "memory_pressure" || gatewaytest.Content(m) == "" {
		t.Errorf("after a memory emergency: %v", m)
	}
}

// Lifecycle events carry ids, never content.
func TestRunLogs(t *testing.T) {
	logs := &gatewaytest.SyncBuffer{}
	gatewaytest.Log = logs
	ts := newTestServer(t)
	gatewaytest.Log = nil
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	id := c.Start("fake:reply secretOutputText42")
	m := c.WaitStatus(id, "completed")
	if gatewaytest.Content(m) != "secretOutputText42 " {
		t.Fatalf("output %q", gatewaytest.Content(m))
	}
	c.Do("DELETE", "/_localbase/runs/"+id, "")
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
		{"GET", "/_localbase/runs/run_1/result", true}, {"POST", "/_localbase/runs/run_1/result", false},
		{"POST", "/_localbase/runs/v1/images/generations", true}, {"POST", "/_localbase/runs/v1/images/edits", true},
		{"POST", "/_localbase/runs/v1/audio/speech", true}, {"POST", "/_localbase/runs/v1/audio/transcriptions", true},
		{"POST", "/_localbase/runs/v1/audio/translations", true}, {"POST", "/_localbase/runs/v1/systemone", true},
		{"POST", "/_localbase/runs/v1/videos", false},
	} {
		if got := allowedUIRoute(c.method, c.path); got != c.ok {
			t.Errorf("%s %s: %v", c.method, c.path, got)
		}
	}
	ts := newTestServer(t)
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	code, m := c.Do("POST", "/app/api/_localbase/runs/v1/chat/completions", gatewaytest.ChatBody("fake:slow 5"))
	if code != 202 {
		t.Fatalf("create through /app/api: %d %v", code, m)
	}
	id := m["id"].(string)
	c.WaitStatus(id, "completed")
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
