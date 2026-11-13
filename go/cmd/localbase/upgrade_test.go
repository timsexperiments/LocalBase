package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/testutil"
)

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
	has := set(runtime.BackendPorts(e.Config, []string{"--root", "/r", "serve", "--tts-port", "28003", "--image-port", "28004"}))
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
	has = set(runtime.BackendPorts(e.Config, []string{"--root", "/r", "serve"}))
	if !has[29501] || len(has) != 1 {
		t.Errorf("only the llm is in use: %v", has)
	}

	// Active without selected still counts.
	e = &env{Config: &store.Config{Port: 18000, Active: map[string]string{"tts": "voice"}}}
	if has = set(runtime.BackendPorts(e.Config, nil)); !has[8092] || len(has) != 1 {
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
