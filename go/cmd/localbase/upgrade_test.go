package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

func TestClassifyGateway(t *testing.T) {
	const binary = "/bin/local-base"
	ok := gatewayFacts{lockState: "active", ownerManaged: true, definitionInstalled: true, reachable: true, serviceProgram: binary, binary: binary}
	with := func(change func(*gatewayFacts)) gatewayFacts {
		f := ok
		change(&f)
		return f
	}
	if state, err := classifyGateway(ok); err != nil || state != gatewayManaged {
		t.Errorf("a managed gateway on this binary: %v %v", state, err)
	}
	for _, lock := range []string{"missing", "stale"} {
		if state, err := classifyGateway(gatewayFacts{lockState: lock}); err != nil || state != gatewayNone {
			t.Errorf("%s: %v %v", lock, state, err)
		}
	}
	for name, tc := range map[string]struct {
		f    gatewayFacts
		want string
	}{
		"foreground":            {with(func(f *gatewayFacts) { f.ownerManaged = false }), "foreground"},
		"manager unreachable":   {with(func(f *gatewayFacts) { f.reachable = false }), "cannot be reached"},
		"no service definition": {with(func(f *gatewayFacts) { f.definitionInstalled = false }), "cannot be reached"},
		"another binary":        {with(func(f *gatewayFacts) { f.serviceProgram = "/other/local-base" }), "not this binary"},
		"an unreadable lock":    {gatewayFacts{lockState: "invalid", lockDetail: "gateway owner record is invalid"}, "Cannot tell"},
		"a lock being set up":   {gatewayFacts{lockState: "unknown"}, "Cannot tell"},
	} {
		_, err := classifyGateway(tc.f)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
		if err != nil && !strings.Contains(err.Error(), "Nothing was changed") {
			t.Errorf("%s: it should say nothing changed: %v", name, err)
		}
	}
}

func TestWaitIdle(t *testing.T) {
	noSleep := func(time.Duration) {}
	if err := waitIdle(func() (bool, string, error) { return false, "", nil }, 0, noSleep); err != nil {
		t.Errorf("idle: %v", err)
	}
	busy := func() (bool, string, error) { return true, "1 in flight, 0 queued, 0 detached runs", nil }
	err := waitIdle(busy, 0, noSleep)
	if err == nil || !strings.Contains(err.Error(), "--wait") || !strings.Contains(err.Error(), "--force") || !strings.Contains(err.Error(), "1 in flight") {
		t.Errorf("busy, no wait: %v", err)
	}
	// Finishes in time: goes on.
	left := 3
	calls := 0
	err = waitIdle(func() (bool, string, error) { calls++; left--; return left > 0, "x", nil }, time.Minute, noSleep)
	if err != nil || calls != 3 {
		t.Errorf("in time: %v after %d asks", err, calls)
	}
	// Does not: refuses, having cut nothing.
	err = waitIdle(busy, time.Millisecond, func(time.Duration) { time.Sleep(2 * time.Millisecond) })
	if err == nil || !strings.Contains(err.Error(), "nothing was cut") {
		t.Errorf("not in time: %v", err)
	}
	// Cannot ask: it does not guess.
	err = waitIdle(func() (bool, string, error) { return false, "", errors.New("refused") }, 0, noSleep)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("cannot ask: %v", err)
	}
}

func TestBackendPorts(t *testing.T) {
	e := &env{Config: &store.Config{Port: 18000, SttPort: 18080}}
	got := backendPorts(e, []string{"--root", "/r", "serve", "--tts-port", "28003", "--image-port", "28004"})
	has := map[int]bool{}
	for _, p := range got {
		has[p] = true
	}
	for _, want := range []int{18000, 18080, 28003, 28004, 8091} {
		if !has[want] {
			t.Errorf("%d is missing from %v", want, got)
		}
	}
	if has[8092] || has[8090] {
		t.Errorf("a flag moved these: %v", got)
	}
}

func TestCheckInstalled(t *testing.T) {
	if err := checkInstalled("/opt/homebrew/Cellar/local-base/1/bin/local-base"); err == nil || !strings.Contains(err.Error(), "brew upgrade") {
		t.Errorf("Homebrew: %v", err)
	}
	dir := t.TempDir()
	if err := checkInstalled(filepath.Join(dir, "local-base")); err != nil {
		t.Errorf("a writable directory: %v", err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := checkInstalled(filepath.Join(locked, "local-base")); err == nil || !strings.Contains(err.Error(), "Cannot write") {
		t.Errorf("a read-only directory: %v", err)
	}
}

// The release build never compiles the test key in (DESIGN §33.5).
func TestReleaseBuildSetsNoTestKey(t *testing.T) {
	for _, p := range []string{"../../tools/build.sh", "../../../scripts/release/build.sh", "../../../.github/workflows/release-go.yml"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue // not on this branch yet
		}
		if strings.Contains(string(b), "testKey") {
			t.Errorf("%s sets the test key", p)
		}
	}
}
