package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/version"
)

const versionPkg = "github.com/timsexperiments/LocalBase/go/internal/version"

// buildStamped builds this command with the release flags (tools/build.sh).
func buildStamped(t *testing.T, out string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-ldflags",
		"-X "+versionPkg+".Version=9.8.7 -X "+versionPkg+".Commit=0123456789abcdef -X "+versionPkg+".Date=2026-11-02T10:00:00Z",
		"-o", out, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, b)
	}
}

// The -X flags a release uses reach every place a build says what it is, and
// bare --version stays the number alone for scripts.
func TestVersionStamping(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "local-base")
	buildStamped(t, bin)
	run := func(args ...string) string {
		t.Helper()
		var stdout bytes.Buffer
		cmd := exec.Command(bin, args...)
		cmd.Stdout = &stdout
		if err := cmd.Run(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return strings.TrimSpace(stdout.String())
	}
	if got := run("--version"); got != "9.8.7" {
		t.Errorf("--version: %q", got)
	}
	if got := run("version"); !strings.Contains(got, "9.8.7 (commit 0123456789ab, 2026-11-02T10:00:00Z, go1.") {
		t.Errorf("version: %q", got)
	}
	var env struct {
		Data version.Build `json:"data"`
	}
	line, _, _ := strings.Cut(run("--json", "--version"), "\n")
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatalf("%q: %v", line, err)
	}
	if env.Data.Version != "9.8.7" || env.Data.Commit != "0123456789abcdef" || env.Data.Date != "2026-11-02T10:00:00Z" || !strings.HasPrefix(env.Data.GoVersion, "go1.") {
		t.Errorf("--version --json: %+v", env.Data)
	}
}

// The same source and flags give the same bytes.
func TestBuildIsReproducible(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	buildStamped(t, a)
	buildStamped(t, b)
	x, _ := os.ReadFile(a)
	y, _ := os.ReadFile(b)
	if len(x) == 0 || !bytes.Equal(x, y) {
		t.Errorf("two builds differ: %d and %d bytes", len(x), len(y))
	}
}
