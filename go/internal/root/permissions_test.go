package root

import (
	"os"
	"path/filepath"
	"testing"
)

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestTightenPermissions(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string, m os.FileMode) string {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte("x"), m); err != nil {
			t.Fatal(err)
		}
		os.Chmod(p, m)
		return p
	}
	os.Chmod(dir, 0o755)
	loose := []string{
		"local-base.db", "local-base.db-wal", "local-base.db-shm", "local-base.db.bak-1", "ui-access.json", ".localbase-root.json",
		"runs/run_1.json", "logs/events.jsonl", "logs/events.1.jsonl", "logs/service-stderr.log", "runtime/ui-csrf.key", "auth-profiles.json",
	}
	var paths []string
	for _, rel := range loose {
		paths = append(paths, write(rel, 0o644))
	}
	tool := write("cache/tool", 0o755)
	model := write("models/llm/weights.gguf", 0o644)
	binary := write("bin/runtimes/llama-server", 0o755)
	secret := filepath.Join(t.TempDir(), "elsewhere")
	os.WriteFile(secret, []byte("x"), 0o644)
	os.Symlink(secret, filepath.Join(dir, "link"))

	fail := func(p string, err error) { t.Errorf("%s: %v", p, err) }
	if n := TightenPermissions(dir, fail); n == 0 {
		t.Fatal("changed nothing")
	}
	for _, p := range paths {
		if m := mode(t, p); m != 0o600 {
			t.Errorf("%s: %o", p, m)
		}
	}
	if m := mode(t, tool); m != 0o700 {
		t.Errorf("an owner's execute bit: %o", m)
	}
	for _, d := range []string{dir, filepath.Join(dir, "runs"), filepath.Join(dir, "logs"), filepath.Join(dir, "runtime")} {
		if m := mode(t, d); m != 0o700 {
			t.Errorf("%s: %o", d, m)
		}
	}
	if m := mode(t, model); m != 0o644 {
		t.Errorf("models/ should be left alone: %o", m)
	}
	if m := mode(t, binary); m != 0o755 {
		t.Errorf("bin/ should be left alone: %o", m)
	}
	if m := mode(t, secret); m != 0o644 {
		t.Errorf("followed a symlink out of the root: %o", m)
	}
	if n := TightenPermissions(dir, fail); n != 0 {
		t.Errorf("not idempotent: %d changed", n)
	}
}

// A path it cannot read is reported, and the rest still happens.
func TestTightenPermissionsNeverFails(t *testing.T) {
	failures := 0
	TightenPermissions(filepath.Join(t.TempDir(), "missing"), func(string, error) { failures++ })
	if failures != 1 {
		t.Errorf("a missing root: %d failures", failures)
	}
}
