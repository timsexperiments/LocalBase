package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// On the real binary: a fresh root's database is private, and serve fixes a loose
// older root at startup. Go only: TS leaves its database under the umask.
func TestRootFilesArePrivate(t *testing.T) {
	if *implFlag != "go" && *implFlag != "both" {
		t.Skip("runs the Go gateway")
	}
	tmp := scratchDir(t)
	e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp, noAuth: true}
	if r := e.cli(nil, "--non-interactive", "init"); r.code != 0 {
		t.Fatalf("init: %s", r.stderr)
	}
	db := filepath.Join(e.root, "local-base.db")
	perm := func(p string) os.FileMode {
		t.Helper()
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return info.Mode().Perm()
	}
	if m := perm(db); m != 0o600 {
		t.Errorf("fresh database: %o", m)
	}
	backup := db + ".bak-test"
	mustWriteFile(backup, []byte("copy"), 0o644)
	log := filepath.Join(e.root, "logs", "older.jsonl")
	mustMkdirAll(filepath.Dir(log), 0o755)
	mustWriteFile(log, []byte("x"), 0o644)
	for _, p := range []string{db, backup, log} {
		mustChmod(p, 0o644)
	}
	mustChmod(filepath.Dir(log), 0o755)
	e.serve("--no-auth", "--no-llm", "--no-stt", "--no-tts", "--no-image", "--no-video")
	defer e.stop()
	for _, p := range []string{db, backup, log} {
		if m := perm(p); m != 0o600 {
			t.Errorf("%s: %o", p, m)
		}
	}
	if m := perm(filepath.Dir(log)); m != 0o700 {
		t.Errorf("logs: %o", m)
	}
	events, _ := os.ReadFile(filepath.Join(e.root, "logs", "events.jsonl"))
	if !strings.Contains(string(events), "root.permissions-tightened") {
		t.Errorf("no event for the change:\n%s", events)
	}
	if r := e.do("GET", "/health", ""); r.Status != 200 {
		t.Errorf("health: %d", r.Status)
	}
}
