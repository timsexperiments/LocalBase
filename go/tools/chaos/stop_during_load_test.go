package chaos

import (
	"syscall"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/tools/lab"
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
	if procs, err := workspace.Backends(); err != nil || len(procs) != 0 {
		t.Errorf("a backend outlived the gateway: %+v %v", procs, err)
	}
}
