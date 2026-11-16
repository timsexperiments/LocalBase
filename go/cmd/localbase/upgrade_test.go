package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"regexp"
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
	c := &cli.Ctx{Stdout: io.Discard, Stderr: io.Discard}
	script := func(body string) string {
		p := filepath.Join(t.TempDir(), "local-base")
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	e := &env{Root: r, Config: &store.Config{}, readyWait: 300 * time.Millisecond}

	// The manager accepted it, and nothing ever answers: that is a failed start, so it rolls back.
	start := time.Now()
	err = managedOps(c, e, script("exit 0"), nil).Start()
	if err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Errorf("no gateway came up: %v", err)
	}
	if time.Since(start) < e.readyWait {
		t.Errorf("it did not wait: %v", time.Since(start))
	}

	// A restart that fails says so at once, and nothing waits for it.
	start = time.Now()
	err = managedOps(c, e, script("echo broken >&2; exit 1"), nil).Start()
	if err == nil || !strings.Contains(err.Error(), "broken") || strings.Contains(err.Error(), "did not become ready") {
		t.Errorf("a failed restart: %v", err)
	}
	if time.Since(start) >= e.readyWait {
		t.Errorf("it waited after a failed restart: %v", time.Since(start))
	}
}

// The release build never compiles the test key in (DESIGN §33.5): no -X of it, no
// localbase_testkey tag, no tags through GOFLAGS, and it never builds the upgrade rehearsal, whose
// kit carries the test key. Every file is read: one that moved fails this test rather than
// leaving it to check nothing.
func TestReleaseBuildSetsNoTestKey(t *testing.T) {
	repo := testutil.RepoRoot(t)
	// The module's scripts, by their path from the repository's top (go/scripts/... while the
	// module is in go/, scripts/... at the top), as the release workflow names them.
	module, err := filepath.Rel(repo, testutil.ModuleRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	buildScript := filepath.ToSlash(filepath.Join(module, "scripts", "build.sh"))
	goflagsTags := regexp.MustCompile(`GOFLAGS[^\n]*-tags`)
	for _, p := range []string{buildScript, "scripts/release/build.sh", ".github/workflows/release-go.yml"} {
		b, err := os.ReadFile(filepath.Join(repo, p))
		if err != nil {
			t.Fatalf("the release build's %s: %v", p, err)
		}
		refused := []string{"testKey", "localbase_testkey", "-tags"}
		if p != buildScript { // its comment says the rehearsal reads its flags; below, it builds only the command
			refused = append(refused, "upgrade-rehearsal")
		}
		for _, word := range refused {
			if strings.Contains(string(b), word) {
				t.Errorf("%s names %q", p, word)
			}
		}
		if goflagsTags.Match(b) {
			t.Errorf("%s sets tags through GOFLAGS", p)
		}
		if p == buildScript && (!strings.Contains(string(b), "./cmd/localbase") || strings.Contains(string(b), "./tools/")) {
			t.Errorf("%s builds something other than ./cmd/localbase", p)
		}
	}
	// What the release builds is checked too, by the module's scripts/check-release-artifact.sh,
	// between the build and the step that keeps the binaries. A move of the script, or of the step,
	// fails here rather than leaving the release to check nothing.
	check := filepath.ToSlash(filepath.Join(module, "scripts", "check-release-artifact.sh"))
	if _, err := os.Stat(filepath.Join(repo, check)); err != nil {
		t.Errorf("the release's artifact check: %v", err)
	}
	wf, err := os.ReadFile(filepath.Join(repo, ".github", "workflows", "release-go.yml"))
	if err != nil {
		t.Fatal(err)
	}
	build, run, keep := strings.Index(string(wf), "scripts/release/build.sh binaries"), strings.Index(string(wf), check+` "$RUNNER_TEMP"/out/bin/*`), strings.Index(string(wf), "name: Keep the binaries")
	if build < 0 || run < 0 || keep < 0 || !(build < run && run < keep) {
		t.Errorf("release-go.yml must run %s on every built binary after the build and before keeping them (build %d, check %d, keep %d)", check, build, run, keep)
	}
}

// The test key exists only in a build tagged localbase_testkey: every shipped source file (cmd,
// internal) that names the testKey identifier carries the tag on its first line. Like
// TestLabSeamFilesAreTagged; by identifier, not text, since the untagged file's comment names it.
func TestTestKeyFilesAreTagged(t *testing.T) {
	for _, dir := range []string{".", "../../internal"} {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			named := false
			ast.Inspect(f, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && id.Name == "testKey" {
					named = true
				}
				return !named
			})
			if named && !bytes.HasPrefix(src, []byte("//go:build localbase_testkey\n")) {
				t.Errorf("%s names testKey without //go:build localbase_testkey on its first line", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
