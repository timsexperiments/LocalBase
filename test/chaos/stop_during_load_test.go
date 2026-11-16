package chaos

import (
	"encoding/json"
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/test/lab"
)

// serviceStopLimit is what launchd and systemd give a stopping gateway (ExitTimeOut and
// TimeoutStopSec), after which they kill it. A stop must finish well inside it.
const serviceStopLimit = 15 * time.Second

// A stop during a slow model load ends the load, and does not wait for it, even when the backend
// won't stop until it is killed. The request gets the transient answer.
func TestStopDuringBackendLoad(t *testing.T) {
	const limit = 6 * time.Second // well inside serviceStopLimit
	imageBody := map[string]any{"model": lab.Image, "prompt": "a boat", "n": 1, "size": "512x512"}
	for _, c := range []struct {
		name, path string
		body       any
		env        []string
	}{
		{"chat, stops", chatPath, chatBody(lab.ChatSmall, "hello"), []string{"FAKE_STARTUP_MS=60000"}},
		{"chat, ignores term", chatPath, chatBody(lab.ChatSmall, "hello"), []string{"FAKE_STARTUP_MS=60000", "FAKE_IGNORE_TERM=1"}},
		{"image, ignores term", "/v1/images/generations", imageBody, []string{"FAKE_STARTUP_MS=60000", "FAKE_IGNORE_TERM=1"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, g, client := start(t, lab.Options{FakeEnv: c.env})
			answer := make(chan lab.Call, 1)
			go func() { answer <- client.PostJSON(ctx, c.path, g.Keys.User, c.body) }()
			waitFor(t, "the backend to start loading", 10*time.Second, func() bool {
				return len(events(t, g, "runtime.starting")) > 0
			})
			time.Sleep(300 * time.Millisecond) // it has really started
			began := time.Now()
			if err := g.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			select {
			case r := <-answer:
				if r.Status != 503 {
					t.Errorf("the waiting request: %d %.300s", r.Status, r.Body)
				}
			case <-time.After(limit):
				t.Error("the waiting request got no answer")
			}
			select {
			case <-g.Done():
				took := time.Since(began)
				t.Logf("stopped in %v", took.Round(100*time.Millisecond))
				if took > limit {
					t.Errorf("a stop during a load took %v, against %v and a service manager limit of %v", took, limit, serviceStopLimit)
				}
			case <-time.After(45 * time.Second):
				t.Fatal("still not stopped after 45 s")
			}
			if procs, err := workspace.Backends(); err != nil || len(procs) != 0 {
				t.Errorf("a backend outlived the gateway: %+v %v", procs, err)
			}
		})
	}
}

// Several wedged backends, serving, that ignore SIGTERM and SIGINT: each polite stop would take its
// whole grace, one after another, so only the deadline keeps the stop inside the limit.
func TestStopWithWedgedBackends(t *testing.T) {
	ctx, g, c := start(t, lab.Options{FakeEnv: []string{"FAKE_IGNORE_TERM=1"}})
	mustChat(t, ctx, c, g, lab.ChatSmall)
	image := map[string]any{"model": lab.Image, "prompt": "a boat", "n": 1, "size": "512x512"}
	if r := c.PostJSON(ctx, "/v1/images/generations", g.Keys.User, image); r.Status != 200 {
		t.Fatalf("image: %d %.300s", r.Status, r.Body)
	}
	began := time.Now()
	if err := g.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-g.Done():
		took := time.Since(began)
		t.Logf("stopped in %v", took.Round(100*time.Millisecond))
		if took > serviceStopLimit-time.Second {
			t.Errorf("the stop took %v; the service manager kills at %v", took, serviceStopLimit)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("still not stopped after 45 s")
	}
	if took := time.Since(began); took < 12*time.Second {
		// The backends ignore SIGTERM, so only the kill at 12 s ends them.
		t.Errorf("stopped after %v: wedged backends should hold the stop until the kill at 12 s", took)
	}
	if n := len(events(t, g, "gateway.stop-deadline")); n != 1 {
		t.Errorf("gateway.stop-deadline logged %d times, want once", n)
	}
	if procs, err := workspace.Backends(); err != nil || len(procs) != 0 {
		t.Errorf("a backend outlived the gateway: %+v %v", procs, err)
	}
}

// A request still streaming when the gateway stops has until 9 s, the drain deadline, and is then
// cut. The backend stops when asked, so nothing is killed at 12 s.
func TestStopGivesARunningRequestUntilTheDrain(t *testing.T) {
	ctx, g, c := start(t, lab.Options{})
	mustChat(t, ctx, c, g, lab.ChatSmall)
	var stopped time.Time
	st := c.ChatStream(ctx, g.Keys.User, streamBody(lab.ChatSmall, "fake:slow 400"), func(s *lab.Stream) bool {
		if stopped.IsZero() && s.Chunks >= 3 {
			stopped = time.Now()
			if err := g.Signal(syscall.SIGTERM); err != nil {
				t.Error(err)
			}
		}
		return true
	})
	if stopped.IsZero() {
		t.Fatalf("the stream ended before the stop: %+v", st)
	}
	cut := time.Since(stopped)
	t.Logf("the request was cut %v after the stop", cut.Round(100*time.Millisecond))
	if cut < 8500*time.Millisecond || cut > 10500*time.Millisecond {
		t.Errorf("the request was cut %v after the stop; the drain deadline is 9 s", cut)
	}
	if st.Done {
		t.Error("a request cut by the stop claimed [DONE]")
	}
	select {
	case <-g.Done():
		took := time.Since(stopped)
		t.Logf("stopped in %v", took.Round(100*time.Millisecond))
		if took > serviceStopLimit-time.Second {
			t.Errorf("the stop took %v; the service manager kills at %v", took, serviceStopLimit)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("still not stopped after 45 s")
	}
	if n := len(events(t, g, "gateway.stop-deadline")); n != 0 {
		t.Errorf("a backend that stops when asked was killed at the deadline (%d events)", n)
	}
	if procs, err := workspace.Backends(); err != nil || len(procs) != 0 {
		t.Errorf("a backend outlived the gateway: %+v %v", procs, err)
	}
}

// A gateway stuck in its own stop still leaves: every backend killed at the deadline, then the
// process out, within the limit, saying so. A detached run comes back as interrupted.
func TestStopWhenTheGatewayIsStuck(t *testing.T) {
	ctx, g, c := start(t, lab.Options{FakeEnv: []string{"LOCALBASE_LAB_STOP_STUCK=1"}})
	mustChat(t, ctx, c, g, lab.ChatSmall)
	key := g.Keys.Runs[0]
	create := c.PostJSON(ctx, "/_localbase/runs/v1/chat/completions", key, chatBody(lab.ChatSmall, "fake:slow 2000"))
	if create.Status != 202 {
		t.Fatalf("create: %d %.300s", create.Status, create.Body)
	}
	var run runRecord
	if err := json.Unmarshal(create.Body, &run); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond) // past its first checkpoint
	began := time.Now()
	if err := g.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-g.Done():
	case <-time.After(45 * time.Second):
		t.Fatal("still not stopped after 45 s")
	}
	took := time.Since(began)
	t.Logf("left in %v", took.Round(100*time.Millisecond))
	if took < 13*time.Second || took > serviceStopLimit-time.Second {
		t.Errorf("left after %v, want about 13.5 s", took)
	}
	var exit *exec.ExitError
	if err := g.ExitErr(); !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Errorf("exit: %v, want status 1", err)
	}
	for _, name := range []string{"gateway.stop-deadline", "gateway.stop-abandoned"} {
		if len(events(t, g, name)) != 1 {
			t.Errorf("%s wasn't logged once", name)
		}
	}
	if procs, err := workspace.Backends(); err != nil || len(procs) != 0 {
		t.Errorf("a backend outlived the gateway: %+v %v", procs, err)
	}
	g2, err := workspace.Start(ctx, lab.Options{Name: g.Opts.Name, Reuse: g, CtxSize: 8192})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	t.Cleanup(func() { g2.Stop(30 * time.Second) })
	view := lab.NewClient(g2.Base).Do(ctx, "GET", "/_localbase/runs/"+run.ID, key, "", nil)
	var got runRecord
	if view.Status != 200 || json.Unmarshal(view.Body, &got) != nil {
		t.Fatalf("the run: %d %.300s", view.Status, view.Body)
	}
	if got.Status != "interrupted" || got.Reason != "gateway_restart" {
		t.Errorf("a run across a stuck stop: status %q reason %q", got.Status, got.Reason)
	}
}
