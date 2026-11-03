// Package chaos breaks a real gateway on purpose: it kills backends mid-request,
// makes them crash in loops, kills the gateway itself, locks and corrupts its
// database, and sends it abusive request bodies. Every test states what must
// hold afterward: a documented answer rather than a hang or a 500, and a gateway
// that recovers.
//
//	make chaos
//
// Fake backends and a fake memory host only; see docs/reliability.md.
package chaos

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/tools/lab"
)

var workspace *lab.Workspace

func TestMain(m *testing.M) {
	// Below the machine's other work; every process here inherits it.
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, 0, 10)
	code := func() int {
		w, err := lab.NewWorkspace("")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		defer w.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := w.Build(ctx, os.Getenv("LOCALBASE_CHAOS_RACE") != ""); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		workspace = w
		return m.Run()
	}()
	os.Exit(code)
}

const testLimit = 2 * time.Minute

// start runs a gateway for one test, stopped and checked at its end.
func start(t *testing.T, o lab.Options) (context.Context, *lab.Gateway, *lab.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testLimit)
	t.Cleanup(cancel)
	o.Name = strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	if o.CtxSize == 0 {
		o.CtxSize = 8192
	}
	if o.QueueTimeoutMS == 0 {
		o.QueueTimeoutMS = 10_000
	}
	g, err := workspace.Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		select {
		case <-g.Done():
			// The test ended it, on purpose or with its own check.
		default:
			dumpIfStuck(t, g)
			if took, err := g.Stop(30 * time.Second); err != nil {
				t.Errorf("the gateway did not stop cleanly after %v: %v", took.Round(time.Millisecond), err)
			}
		}
		checkLogClean(t, g)
		workspace.KillStray() // nothing crosses into the next test
	})
	return ctx, g, lab.NewClient(g.Base)
}

// dumpIfStuck saves the goroutines of a gateway still stopping well after a
// SIGTERM, for the failure report. It runs alongside the stop.
func dumpIfStuck(t *testing.T, g *lab.Gateway) {
	t.Helper()
	path := filepath.Join(os.TempDir(), "chaos-stuck-"+strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())+".txt")
	time.AfterFunc(20*time.Second, func() {
		select {
		case <-g.Done():
		default:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := g.GoroutineDump(ctx, path); err == nil {
				fmt.Fprintln(os.Stderr, "chaos: a stuck gateway's goroutines are in", path)
			}
		}
	})
}

// checkLogClean fails on what must never be in any log, whatever was broken.
func checkLogClean(t *testing.T, g *lab.Gateway) {
	t.Helper()
	tail := &lab.LogTail{Path: g.LogPath, Keep: func(lab.Event) bool { return false },
		Seen: func(e lab.Event) {
			if e.Name == "http.panic" {
				t.Errorf("a handler panicked: %s", e.Message)
			}
		},
		Raw: func(line string) {
			if strings.HasPrefix(line, "panic:") || strings.HasPrefix(line, "fatal error:") || strings.HasPrefix(line, "WARNING: DATA RACE") {
				t.Errorf("gateway log: %s", line)
			}
		}}
	if err := tail.Read(); err != nil {
		t.Logf("reading the log: %v", err)
	}
	for _, report := range g.RaceReports() {
		b, _ := os.ReadFile(report)
		t.Errorf("data race:\n%.2000s", b)
	}
}

// events reads the log's events with the given names so far.
func events(t *testing.T, g *lab.Gateway, names ...string) []lab.Event {
	t.Helper()
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	tail := &lab.LogTail{Path: g.LogPath, Keep: func(e lab.Event) bool { return want[e.Name] }}
	if err := tail.Read(); err != nil {
		t.Fatal(err)
	}
	return tail.Events()
}

// waitFor polls cond until it holds, failing the test at the limit.
func waitFor(t *testing.T, what string, limit time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited %v for %s", limit, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func backendNamed(t *testing.T, name string) lab.Proc {
	t.Helper()
	procs, err := workspace.Backends()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range procs {
		if p.Command == name {
			return p
		}
	}
	t.Fatalf("no %s running: %+v", name, procs)
	return lab.Proc{}
}

func killBackend(t *testing.T, name string) lab.Proc {
	t.Helper()
	p := backendNamed(t, name)
	if err := syscall.Kill(p.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	return p
}

func chatBody(model, content string) map[string]any {
	return map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": content}}}
}

func streamBody(model, content string) map[string]any {
	b := chatBody(model, content)
	b["stream"] = true
	return b
}

const chatPath = "/v1/chat/completions"

// mustChat is a request that has to work now.
func mustChat(t *testing.T, ctx context.Context, c *lab.Client, g *lab.Gateway, model string) {
	t.Helper()
	r := c.PostJSON(ctx, chatPath, g.Keys.User, chatBody(model, "hello"))
	if r.Err != nil || r.Status != 200 {
		t.Fatalf("chat with %s: %d %v %.300s", model, r.Status, r.Err, r.Body)
	}
}

// active is the LLM's admitted requests, as the gateway reports them.
func active(ctx context.Context, c *lab.Client, g *lab.Gateway, model string) int {
	r := c.Do(ctx, "GET", "/_localbase/models/"+model, g.Keys.User, "", nil)
	var m struct {
		Device struct {
			Runtime *struct {
				Active *int `json:"activeAdmissions"`
			} `json:"runtime"`
		} `json:"device"`
	}
	if r.Err != nil || r.Status != 200 || json.Unmarshal(r.Body, &m) != nil || m.Device.Runtime == nil || m.Device.Runtime.Active == nil {
		return -1
	}
	return *m.Device.Runtime.Active
}

// transient checks a failure follows the contract for transient failures: 503
// or 429, with an error envelope and Retry-After (DESIGN §13.6).
func transient(t *testing.T, what string, r lab.Call) {
	t.Helper()
	if r.Err != nil {
		t.Errorf("%s: connection error instead of an answer: %v", what, r.Err)
		return
	}
	if (r.Status != 503 && r.Status != 429) || r.Code == "" || r.RetryAfter == "" {
		t.Errorf("%s: %d code %q Retry-After %q, expected 503 or 429 with both: %.300s", what, r.Status, r.Code, r.RetryAfter, r.Body)
	}
}

// knownBug skips a test that fails because of an open bug, unless
// LOCALBASE_CHAOS_ALL is set (make chaos-all). It names the bug and the pull request
// that fixes it; that fix removes the line, and the test guards it from then on.
// tools/known-bugs-check.sh fails once a named pull request has merged, so a skip
// cannot outlive its bug unnoticed. noPRYet says there is no fix yet, and fails that
// check too.
func knownBug(t *testing.T, pr int, bug string) {
	t.Helper()
	if os.Getenv("LOCALBASE_CHAOS_ALL") == "" {
		t.Skipf("known bug, fixed by #%d: %s", pr, bug)
	}
}

const noPRYet = 0 //nolint:unused // for a new bug that has no PR yet; tools/known-bugs-check.sh fails on it

// --- a backend dies mid-request -------------------------------------------------

// A stream whose backend is killed ends promptly with an error event, not a
// hang or a false [DONE], and the next request starts the model again.
func TestBackendKilledMidStream(t *testing.T) {
	ctx, g, c := start(t, lab.Options{})
	mustChat(t, ctx, c, g, lab.ChatSmall)
	killed := false
	began := time.Now()
	st := c.ChatStream(ctx, g.Keys.User, streamBody(lab.ChatSmall, "fake:slow 400"), func(s *lab.Stream) bool {
		if !killed && s.Chunks >= 3 {
			killed = true
			killBackend(t, "llama-server")
		}
		return true
	})
	if !killed {
		t.Fatalf("the stream ended before the kill: %+v", st)
	}
	if took := time.Since(began); took > 10*time.Second {
		t.Errorf("the client waited %v after its backend died", took)
	}
	if st.Done {
		t.Errorf("a stream cut by a crash claimed [DONE]")
	}
	if st.ErrorEvent == "" {
		t.Errorf("a stream cut by a crash has no error event: %+v", st.Call)
	}
	waitFor(t, "runtime.exited", 5*time.Second, func() bool { return len(events(t, g, "runtime.exited")) == 1 })
	mustChat(t, ctx, c, g, lab.ChatSmall)
}

// A request waiting on a backend that is killed gets the transient-failure
// answer, and the next one works.
func TestBackendKilledMidRequest(t *testing.T) {
	knownBug(t, 477, "a crash mid-request answers 502 without Retry-After")
	ctx, g, c := start(t, lab.Options{FakeDelayMS: 1500})
	mustChat(t, ctx, c, g, lab.ChatSmall)
	done := make(chan lab.Call, 1)
	go func() { done <- c.PostJSON(ctx, chatPath, g.Keys.User, chatBody(lab.ChatSmall, "hello")) }()
	waitFor(t, "the request to be admitted", 5*time.Second, func() bool { return active(ctx, c, g, lab.ChatSmall) == 1 })
	time.Sleep(100 * time.Millisecond) // it has reached the backend
	killBackend(t, "llama-server")
	transient(t, "a request whose backend died", <-done)
	mustChat(t, ctx, c, g, lab.ChatSmall)
}

// The same for the media runtimes.
func TestMediaBackendKilledMidRequest(t *testing.T) {
	knownBug(t, 477, "a crash mid-request answers 502 without Retry-After")
	ctx, g, c := start(t, lab.Options{})
	image := func(prompt string) lab.Call {
		return c.PostJSON(ctx, "/v1/images/generations", g.Keys.User, map[string]any{"model": lab.Image, "prompt": prompt, "n": 1, "size": "512x512"})
	}
	if r := image("a boat"); r.Status != 200 {
		t.Fatalf("first image: %d %.300s", r.Status, r.Body)
	}
	done := make(chan lab.Call, 1)
	go func() { done <- image("fake:slow-image") }()
	time.Sleep(400 * time.Millisecond) // well inside the 1.5 s the fake takes
	killBackend(t, "sd-server")
	transient(t, "an image whose backend died", <-done)
	if r := image("a boat"); r.Status != 200 {
		t.Errorf("the image runtime did not come back: %d %.300s", r.Status, r.Body)
	}
}

// A detached run whose backend dies ends as failed with what it had, and its
// event stream ends; a page following it is never left waiting.
func TestDetachedRunBackendKilled(t *testing.T) {
	ctx, g, c := start(t, lab.Options{})
	mustChat(t, ctx, c, g, lab.ChatSmall)
	key := g.Keys.Runs[0]
	create := c.PostJSON(ctx, "/_localbase/runs/v1/chat/completions", key, chatBody(lab.ChatSmall, "fake:slow 400"))
	if create.Status != 202 {
		t.Fatalf("create: %d %.300s", create.Status, create.Body)
	}
	var run struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Output *struct {
			Content string `json:"content"`
		} `json:"output"`
		Error map[string]any `json:"error"`
	}
	json.Unmarshal(create.Body, &run)
	read := func() {
		r := c.Do(ctx, "GET", "/_localbase/runs/"+run.ID, key, "", nil)
		if r.Status != 200 || json.Unmarshal(r.Body, &run) != nil {
			t.Fatalf("read: %d %.300s", r.Status, r.Body)
		}
	}
	waitFor(t, "output", 10*time.Second, func() bool {
		read()
		return run.Output != nil && strings.Count(run.Output.Content, " ") >= 5
	})
	killBackend(t, "llama-server")
	waitFor(t, "the run to end", 10*time.Second, func() bool {
		read()
		return run.Status != "in_progress" && run.Status != "queued"
	})
	if run.Status != "failed" || run.Error == nil {
		t.Errorf("a run whose backend died: status %q error %v", run.Status, run.Error)
	}
	if run.Output == nil || !strings.HasPrefix(run.Output.Content, "w1 w2 w3 ") {
		t.Errorf("the partial output was lost: %+v", run.Output)
	}
	events := c.Do(ctx, "GET", "/_localbase/runs/"+run.ID+"/events", key, "", nil)
	if !strings.Contains(string(events.Body), "event: end") {
		t.Errorf("the event stream has no end: %d %.300s", events.Status, events.Body)
	}
	mustChat(t, ctx, c, g, lab.ChatSmall)
}

// --- crash loops ----------------------------------------------------------------

// settledStats is a resource sample once connections have closed.
func settledStats(t *testing.T, ctx context.Context, g *lab.Gateway, c *lab.Client) lab.Stats {
	t.Helper()
	c.CloseIdle()
	time.Sleep(300 * time.Millisecond)
	s, err := g.Settled(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func checkGrowth(t *testing.T, first, last lab.Stats, goroutines, fds int) {
	t.Helper()
	if last.Goroutines-first.Goroutines > goroutines {
		t.Errorf("goroutines went from %d to %d, more than the %d allowed", first.Goroutines, last.Goroutines, goroutines)
	}
	if last.FDs-first.FDs > fds {
		t.Errorf("descriptors went from %d to %d, more than the %d allowed", first.FDs, last.FDs, fds)
	}
}

// A backend that cannot start: every request gets the transient-failure answer
// without hanging, nothing piles up, nothing respawns on its own, and once it
// can start again the gateway needs no restart.
func TestStartupCrashLoop(t *testing.T) {
	failFile := filepath.Join(t.TempDir(), "fail")
	if err := os.WriteFile(failFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, g, c := start(t, lab.Options{FakeEnv: []string{"FAKE_FAIL_FILE=" + failFile}})
	first := settledStats(t, ctx, g, c)
	for i := range 25 {
		began := time.Now()
		r := c.PostJSON(ctx, chatPath, g.Keys.User, chatBody(lab.ChatSmall, "hello"))
		transient(t, fmt.Sprintf("request %d to a backend that cannot start", i), r)
		if r.Code != "backend_startup_failed" && r.Code != "service_unavailable" {
			t.Errorf("request %d: code %q", i, r.Code)
		}
		if took := time.Since(began); took > 5*time.Second {
			t.Errorf("request %d took %v to fail", i, took)
		}
	}
	// Concurrent requests share the failure; none is left waiting.
	results := make(chan lab.Call, 6)
	for range 6 {
		go func() { results <- c.PostJSON(ctx, chatPath, g.Keys.User, chatBody(lab.ChatSmall, "hello")) }()
	}
	for range 6 {
		transient(t, "a concurrent request to a backend that cannot start", <-results)
	}
	if procs, _ := workspace.Backends(); len(procs) != 0 {
		t.Errorf("backends running between failures: %+v", procs)
	}
	time.Sleep(time.Second)
	if n := len(events(t, g, "runtime.starting")); n > 31 {
		t.Errorf("%d start attempts for 31 requests: it respawns on its own", n)
	}
	if n := lab.Zombies(g.Cmd.Process.Pid); n != 0 {
		t.Errorf("%d unreaped children", n)
	}
	checkGrowth(t, first, settledStats(t, ctx, g, c), 6, 4)
	if err := os.Remove(failFile); err != nil {
		t.Fatal(err)
	}
	mustChat(t, ctx, c, g, lab.ChatSmall)
}

// A backend that dies after each request: the next request brings it back every
// time, with no hang and no growth.
func TestCrashAfterEveryRequest(t *testing.T) {
	knownBug(t, 477, "a crash mid-request answers 502 without Retry-After")
	ctx, g, c := start(t, lab.Options{})
	mustChat(t, ctx, c, g, lab.ChatSmall)
	first := settledStats(t, ctx, g, c)
	const rounds = 20
	failures := 0
	for i := range rounds {
		killBackend(t, "llama-server")
		// A request can reach the dead backend before the gateway notices; it
		// must still be a clean answer, and the one after it must work.
		r := c.PostJSON(ctx, chatPath, g.Keys.User, chatBody(lab.ChatSmall, "hello"))
		if r.Err != nil || r.Status != 200 {
			failures++
			transient(t, fmt.Sprintf("round %d", i), r)
			mustChat(t, ctx, c, g, lab.ChatSmall)
		}
	}
	t.Logf("%d of %d requests met the dead backend", failures, rounds)
	if n := lab.Zombies(g.Cmd.Process.Pid); n != 0 {
		t.Errorf("%d unreaped children", n)
	}
	if n := len(events(t, g, "runtime.exited")); n != rounds {
		t.Errorf("%d runtime.exited events for %d kills", n, rounds)
	}
	checkGrowth(t, first, settledStats(t, ctx, g, c), 8, 6)
}

// A switch to a model that cannot start leaves the previous one serving.
func TestFailedSwitchRestoresPrevious(t *testing.T) {
	ctx, g, c := start(t, lab.Options{FakeEnv: []string{"FAKE_FAIL_MODEL=" + lab.PrimaryFile(lab.ChatLarge)}})
	mustChat(t, ctx, c, g, lab.ChatSmall)
	r := c.PostJSON(ctx, chatPath, g.Keys.User, chatBody(lab.ChatLarge, "hello"))
	transient(t, "a switch to a model that cannot start", r)
	mustChat(t, ctx, c, g, lab.ChatSmall)
	if n := len(events(t, g, "runtime.switch-rollback")); n != 1 {
		t.Errorf("%d rollback events", n)
	}
	// It stays the default: the failed model did not take the active slot.
	omitted := c.PostJSON(ctx, chatPath, g.Keys.User, map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if omitted.Status == 200 && !strings.Contains(string(omitted.Body), lab.PrimaryFile(lab.ChatSmall)) {
		t.Errorf("the default model changed after a failed switch: %.300s", omitted.Body)
	}
}

// --- the gateway itself dies -------------------------------------------------

// A gateway killed outright leaves no backend behind that stops its successor
// from serving: the restart answers from a backend it owns, on the same ports.
func TestGatewayKilledOutright(t *testing.T) {
	ctx, g, c := start(t, lab.Options{})
	mustChat(t, ctx, c, g, lab.ChatSmall)
	before := backendNamed(t, "llama-server")
	if err := g.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	<-g.Done()
	g2, err := workspace.Start(ctx, lab.Options{Name: g.Opts.Name, Reuse: g, SamePorts: true, CtxSize: 8192, QueueTimeoutMS: 10_000})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	t.Cleanup(func() { g2.Stop(30 * time.Second); workspace.KillStray() })
	c2 := lab.NewClient(g2.Base)
	r := c2.PostJSON(ctx, chatPath, g2.Keys.User, chatBody(lab.ChatSmall, "hello"))
	if r.Err != nil || r.Status != 200 {
		t.Errorf("first request after the restart: %d %v %.300s", r.Status, r.Err, r.Body)
	}
	procs, _ := workspace.Backends()
	for _, p := range procs {
		if p.PID == before.PID {
			t.Errorf("the dead gateway's backend (pid %d) is still running; parent now %d", p.PID, p.PPID)
		}
		if p.PPID != g2.Cmd.Process.Pid {
			t.Errorf("a backend the new gateway does not own: %+v", p)
		}
	}
	if len(procs) != 1 {
		t.Errorf("%d backends after the restart: %+v", len(procs), procs)
	}
	// Above all, a request is answered by the model it asked for. The response's
	// model field cannot show this, since the gateway writes the requested id
	// there; the fake backend names its file in the text.
	large := c2.PostJSON(ctx, chatPath, g2.Keys.User, chatBody(lab.ChatLarge, "hello"))
	if large.Status != 200 || !strings.Contains(string(large.Body), lab.PrimaryFile(lab.ChatLarge)) {
		t.Errorf("asked for %s after the restart: %d %.300s", lab.ChatLarge, large.Status, large.Body)
	}
	if again := c2.PostJSON(ctx, chatPath, g2.Keys.User, chatBody(lab.ChatSmall, "hello")); again.Status != 200 || !strings.Contains(string(again.Body), lab.PrimaryFile(lab.ChatSmall)) {
		t.Errorf("asked for %s after the restart: %d %.300s", lab.ChatSmall, again.Status, again.Body)
	}
	// The new gateway really controls what serves: an unload stops it.
	unload := c2.Do(ctx, "POST", "/_localbase/models/"+lab.ChatSmall+"/unload", g2.Keys.Admin, "", nil)
	if unload.Status != 200 {
		t.Errorf("unload after the restart: %d %.300s", unload.Status, unload.Body)
	}
	waitFor(t, "the backend to stop after an unload", 12*time.Second, func() bool {
		left, _ := workspace.Backends()
		return len(left) == 0
	})
}
