package root

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// writeLease puts an operation lease on disk in the format the Go and TS CLIs share:
// <coordination dir>/<root hash>.operation.lock/owner.json. A raw body that isn't a record stands for
// one that can't be read.
func writeLease(t *testing.T, canonicalRoot string, pid int, operation string, raw string) {
	t.Helper()
	lock := filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "local-base", Hash(canonicalRoot)+".operation.lock")
	if err := os.MkdirAll(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte(raw)
	if raw == "" {
		var err error
		body, err = json.Marshal(map[string]any{"version": 1, "operationId": "0e1f6f2c-0000-4000-8000-000000000001",
			"operation": operation, "root": canonicalRoot, "rootHash": Hash(canonicalRoot), "pid": pid,
			"startedAt": "2026-11-07T00:00:00.000Z"})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(lock, "owner.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// deadPID is a process that has exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// livePID is another process that is still running, until the test ends.
func livePID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd.Process.Pid
}

// restoreRunning and InUse read the operation lease the way they always have: a lease whose process
// is gone isn't running, one whose record can't be read is only in use, and this process's own lease
// doesn't make the root in use for it.
func TestHowRootReadsTheOperationLease(t *testing.T) {
	for _, c := range []struct {
		name        string
		lease       func(t *testing.T, dir string)
		running, in bool
	}{
		{"no lease", func(*testing.T, string) {}, false, false},
		{"a stale restore", func(t *testing.T, dir string) { writeLease(t, dir, deadPID(t), "restore", "") }, false, false},
		{"another process's restore", func(t *testing.T, dir string) { writeLease(t, dir, livePID(t), "restore", "") }, true, true},
		{"another process's other operation", func(t *testing.T, dir string) { writeLease(t, dir, livePID(t), "configure", "") }, false, true},
		{"this process's restore", func(t *testing.T, dir string) { writeLease(t, dir, os.Getpid(), "restore", "") }, true, false},
		{"a record that can't be read", func(t *testing.T, dir string) { writeLease(t, dir, 0, "", "{ not json") }, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			c.lease(t, dir)
			if got := restoreRunning(dir); got != c.running {
				t.Errorf("restoreRunning: %v, want %v", got, c.running)
			}
			if got := InUse(dir); got != c.in {
				t.Errorf("InUse: %v, want %v", got, c.in)
			}
		})
	}
}
