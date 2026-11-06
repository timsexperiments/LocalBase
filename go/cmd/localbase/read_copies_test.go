package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/root"
)

// readCopyFixture makes a root and a copy of its database in the cache folder. held keeps a lock on it,
// as a live reader does. The pid is a live one, so only the lock says whether its reader has gone.
func readCopyFixture(t *testing.T, rootDir, name string, held bool) string {
	t.Helper()
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := root.Canonical(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(cache, "local-base", fmt.Sprintf("localbase-read-%d-%s-%s", os.Getppid(), root.Hash(canonical)[:16], name))
	if err := os.MkdirAll(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "local-base.db"), []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(stage, ".owner"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	if held {
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatal(err)
		}
	} else {
		_ = lock.Close() // nobody holds it
	}
	return stage
}

func readCopyRoot(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("LOCALBASE_ROOT", "")
	r := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", r, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	return r
}

func TestDoctorNamesALeftReadCopyAndLeavesIt(t *testing.T) {
	r := readCopyRoot(t)
	left := readCopyFixture(t, r, "left", false)
	held := readCopyFixture(t, r, "held", true)
	if code, out := runApp(t, "--root", r, "doctor"); code != 0 {
		t.Fatalf("doctor: %s", out)
	} else {
		shown := root.ShellQuote(left)
		want := "An interrupted read left a private copy of this root's database at " + shown + " (10 bytes). It may hold secrets. To delete it: rm -rf " + shown
		if !strings.Contains(out, want) {
			t.Errorf("doctor doesn't say %q:\n%s", want, out)
		}
		if strings.Contains(out, held) {
			t.Errorf("doctor named a live reader's copy:\n%s", out)
		}
		if strings.Contains(out, "0123456789") {
			t.Error("doctor printed the copy's contents")
		}
	}
	if _, err := os.Stat(left); err != nil {
		t.Errorf("doctor deleted it: %v", err)
	}
	_, out := runApp(t, "--root", r, "--json", "doctor")
	first := out[:strings.Index(out, "\n")]
	var envelope struct {
		Data struct {
			Left []struct {
				Path  string `json:"path"`
				Bytes int64  `json:"bytes"`
			} `json:"leftoverReadCopies"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(first), &envelope); err != nil {
		t.Fatalf("%v: %s", err, first)
	}
	if len(envelope.Data.Left) != 1 || envelope.Data.Left[0].Path != left || envelope.Data.Left[0].Bytes != 10 {
		t.Errorf("JSON: %+v", envelope.Data.Left)
	}
}

func TestUninstallRemovesThisRootsLeftReadCopiesOnly(t *testing.T) {
	r := readCopyRoot(t)
	one := readCopyFixture(t, r, "one", false)
	two := readCopyFixture(t, r, "two", false)
	held := readCopyFixture(t, r, "held", true)
	elsewhere := filepath.Join(t.TempDir(), "another")
	if code, out := runApp(t, "--root", elsewhere, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	other := readCopyFixture(t, elsewhere, "other", false)

	code, out := runApp(t, "--root", r, "uninstall", "--yes")
	if code != 0 {
		t.Fatalf("uninstall: %s", out)
	}
	for _, gone := range []string{one, two} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s stayed: %v", gone, err)
		}
	}
	for _, stays := range []string{held, other} {
		if _, err := os.Stat(stays); err != nil {
			t.Errorf("%s went: %v", stays, err)
		}
	}
	if !strings.Contains(out, "Removed 2 private copies of this root's database left by interrupted reads.") {
		t.Errorf("no count:\n%s", out)
	}
	if !strings.Contains(out, "A LocalBase command still reading this root has a private copy at "+root.ShellQuote(held)+". It's left on purpose: that command removes it when it ends.") {
		t.Errorf("no line for the live reader:\n%s", out)
	}
	if strings.Contains(out, other) {
		t.Errorf("it named another root's copy:\n%s", out)
	}
}

func TestUninstallSaysOneCopyInTheSingular(t *testing.T) {
	r := readCopyRoot(t)
	readCopyFixture(t, r, "one", false)
	code, out := runApp(t, "--root", r, "--json", "uninstall", "--yes")
	if code != 0 {
		t.Fatalf("uninstall: %s", out)
	}
	if !strings.Contains(out, "Removed 1 private copy of this root's database left by an interrupted read.") {
		t.Errorf("no singular:\n%s", out)
	}
	if !strings.Contains(out, `"readCopiesRemoved":1`) {
		t.Errorf("no JSON count:\n%s", out)
	}
}
