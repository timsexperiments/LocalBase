package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/testutil"
)

func TestClassifyGateway(t *testing.T) {
	const binary = "/bin/local-base"
	ok := GatewayFacts{LockState: "active", OwnerManaged: true, DefinitionInstalled: true, Reachable: true, ServiceProgram: binary, Binary: binary}
	with := func(change func(*GatewayFacts)) GatewayFacts {
		f := ok
		change(&f)
		return f
	}
	if state, err := ClassifyGateway(ok); err != nil || state != GatewayManaged {
		t.Errorf("a managed gateway on this binary: %v %v", state, err)
	}
	for _, lock := range []string{"missing", "stale"} {
		if state, err := ClassifyGateway(GatewayFacts{LockState: lock}); err != nil || state != GatewayNone {
			t.Errorf("%s: %v %v", lock, state, err)
		}
	}
	for name, tc := range map[string]struct {
		f    GatewayFacts
		want string
	}{
		"foreground":            {with(func(f *GatewayFacts) { f.OwnerManaged = false }), "foreground"},
		"manager unreachable":   {with(func(f *GatewayFacts) { f.Reachable = false }), "cannot be reached"},
		"no service definition": {with(func(f *GatewayFacts) { f.DefinitionInstalled = false }), "cannot be reached"},
		"another binary":        {with(func(f *GatewayFacts) { f.ServiceProgram = "/other/local-base" }), "not this binary"},
		"an unreadable lock":    {GatewayFacts{LockState: "invalid", LockDetail: "gateway owner record is invalid"}, "Cannot tell"},
		"a lock being set up":   {GatewayFacts{LockState: "unknown"}, "Cannot tell"},
	} {
		_, err := ClassifyGateway(tc.f)
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
	if err := WaitIdle(func() (bool, string, error) { return false, "", nil }, 0, noSleep); err != nil {
		t.Errorf("idle: %v", err)
	}
	busy := func() (bool, string, error) { return true, "1 in flight, 0 queued, 0 detached runs", nil }
	err := WaitIdle(busy, 0, noSleep)
	if err == nil || !strings.Contains(err.Error(), "--wait") || !strings.Contains(err.Error(), "--force") || !strings.Contains(err.Error(), "1 in flight") {
		t.Errorf("busy, no wait: %v", err)
	}
	// Finishes in time: goes on.
	left := 3
	calls := 0
	err = WaitIdle(func() (bool, string, error) { calls++; left--; return left > 0, "x", nil }, time.Minute, noSleep)
	if err != nil || calls != 3 {
		t.Errorf("in time: %v after %d asks", err, calls)
	}
	// Does not: refuses, having cut nothing.
	err = WaitIdle(busy, time.Millisecond, func(time.Duration) { time.Sleep(2 * time.Millisecond) })
	if err == nil || !strings.Contains(err.Error(), "nothing was cut") {
		t.Errorf("not in time: %v", err)
	}
	// Cannot ask: it does not guess.
	err = WaitIdle(func() (bool, string, error) { return false, "", errors.New("refused") }, 0, noSleep)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("cannot ask: %v", err)
	}
}

func TestBackendPorts(t *testing.T) {
	uses := func(kinds ...string) map[string][]string {
		out := map[string][]string{}
		for _, k := range kinds {
			out[k] = []string{"some-" + k + "-model"}
		}
		return out
	}
	set := func(ports []int) map[int]bool {
		has := map[int]bool{}
		for _, p := range ports {
			has[p] = true
		}
		return has
	}

	e := &env{Config: &store.Config{Port: 18000, SttPort: 18080, Selected: uses("llm", "stt", "tts", "image", "video")}}
	has := set(backendPorts(e, []string{"--root", "/r", "serve", "--tts-port", "28003", "--image-port", "28004"}))
	for _, want := range []int{18000, 18080, 28003, 28004, 8091} {
		if !has[want] {
			t.Errorf("%d is missing from %v", want, has)
		}
	}
	if has[8092] || has[8090] {
		t.Errorf("a flag moved these: %v", has)
	}

	// A root with no media model never started a media backend, so another root's on that
	// port must not read as a survivor of this one. This refused a real upgrade otherwise.
	e = &env{Config: &store.Config{Port: 29501, SttPort: 29502, Selected: uses("llm")}}
	has = set(backendPorts(e, []string{"--root", "/r", "serve"}))
	if !has[29501] || len(has) != 1 {
		t.Errorf("only the llm is in use: %v", has)
	}

	// Active without selected still counts.
	e = &env{Config: &store.Config{Port: 18000, Active: map[string]string{"tts": "voice"}}}
	if has = set(backendPorts(e, nil)); !has[8092] || len(has) != 1 {
		t.Errorf("an active model counts: %v", has)
	}
}

// A real service manager returns from restart before the gateway listens. Found by
// tools/upgrade-rehearsal under systemd: smoke failed on a good release.
func TestManagedStartWaitsForReadiness(t *testing.T) {
	r, err := root.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	was := upgradeReadyWait
	upgradeReadyWait = 300 * time.Millisecond
	t.Cleanup(func() { upgradeReadyWait = was })
	c := &cli.Ctx{Stdout: io.Discard, Stderr: io.Discard}
	script := func(body string) string {
		p := filepath.Join(t.TempDir(), "local-base")
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	e := &env{Root: r, Config: &store.Config{}}

	// The manager accepted it, and nothing ever answers: that is a failed start, so it rolls back.
	start := time.Now()
	err = managedOps(c, e, script("exit 0"), nil).Start()
	if err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Errorf("no gateway came up: %v", err)
	}
	if time.Since(start) < upgradeReadyWait {
		t.Errorf("it did not wait: %v", time.Since(start))
	}

	// A restart that fails says so at once, and nothing waits for it.
	start = time.Now()
	err = managedOps(c, e, script("echo broken >&2; exit 1"), nil).Start()
	if err == nil || !strings.Contains(err.Error(), "broken") || strings.Contains(err.Error(), "did not become ready") {
		t.Errorf("a failed restart: %v", err)
	}
	if time.Since(start) >= upgradeReadyWait {
		t.Errorf("it waited after a failed restart: %v", time.Since(start))
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

// The release build never compiles the test key in (DESIGN §33.5). Every file is read: one that
// moved fails this test rather than leaving it to check nothing.
func TestReleaseBuildSetsNoTestKey(t *testing.T) {
	repo := testutil.RepoRoot(t)
	for _, p := range []string{"go/tools/build.sh", "scripts/release/build.sh", ".github/workflows/release-go.yml"} {
		b, err := os.ReadFile(filepath.Join(repo, p))
		if err != nil {
			t.Fatalf("the release build's %s: %v", p, err)
		}
		if strings.Contains(string(b), "testKey") {
			t.Errorf("%s sets the test key", p)
		}
	}
}
