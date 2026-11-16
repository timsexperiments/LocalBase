package gateway

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scratchDir is t.TempDir for a gateway's root, except that a failed removal
// says what was left. t.TempDir only reports "directory not empty", which is
// all that TestUnloadScope's one failure left to go on (DESIGN §24). Like
// t.TempDir, its cleanup runs after those registered later, so everything the
// test started has been stopped by then.
func scratchDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "lbgw-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if msg := removeScratch(dir, os.RemoveAll, 500*time.Millisecond); msg != "" {
			t.Error(msg)
		}
	})
	return dir
}

// removeScratch removes dir, and when that fails returns what is left: what
// remove could not delete, or what appeared while it ran. A second try after
// pause tells a late writer from a file that cannot be deleted.
func removeScratch(dir string, remove func(string) error, pause time.Duration) string {
	err := remove(dir)
	if err == nil {
		return ""
	}
	msg := fmt.Sprintf("removing the scratch root: %v\nleft behind:\n%s", err, leftovers(dir, time.Now()))
	time.Sleep(pause)
	if again := remove(dir); again != nil {
		return msg + fmt.Sprintf("still failing after %v: %v", pause, again)
	}
	return msg + fmt.Sprintf("gone on a retry after %v, so something wrote late", pause)
}

// leftovers lists what is in dir: paths, sizes and ages, never contents.
func leftovers(dir string, now time.Time) string {
	const limit = 40
	var lines []string
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			lines = append(lines, fmt.Sprintf("  %s: %v", path, err))
			return nil
		}
		if path == dir {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		if info, err := d.Info(); err == nil && !d.IsDir() {
			lines = append(lines, fmt.Sprintf("  %s (%d bytes, written %v ago)", rel, info.Size(), now.Sub(info.ModTime()).Round(time.Millisecond)))
		} else {
			lines = append(lines, "  "+rel+"/")
		}
		return nil
	})
	if len(lines) > limit {
		lines = append(lines[:limit], fmt.Sprintf("  ... and %d more", len(lines)-limit))
	}
	if len(lines) == 0 {
		return "  nothing (it had gone by the time of this listing)\n"
	}
	return strings.Join(lines, "\n") + "\n"
}

func TestRemoveScratch(t *testing.T) {
	make1 := func(t *testing.T) string {
		dir := filepath.Join(t.TempDir(), "scratch")
		os.MkdirAll(filepath.Join(dir, "root", "runtime"), 0o700)
		os.WriteFile(filepath.Join(dir, "root", "local-base.db-wal"), []byte("12345"), 0o600)
		return dir
	}
	t.Run("removed", func(t *testing.T) {
		dir := make1(t)
		if msg := removeScratch(dir, os.RemoveAll, 0); msg != "" {
			t.Errorf("message: %s", msg)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("still there: %v", err)
		}
	})
	t.Run("a late writer", func(t *testing.T) {
		dir := make1(t)
		first := true
		remove := func(p string) error {
			if first {
				first = false
				return fmt.Errorf("unlinkat %s: directory not empty", p)
			}
			return os.RemoveAll(p)
		}
		msg := removeScratch(dir, remove, 0)
		for _, want := range []string{"directory not empty", filepath.Join("root", "local-base.db-wal") + " (5 bytes, written", "gone on a retry"} {
			if !strings.Contains(msg, want) {
				t.Errorf("missing %q in:\n%s", want, msg)
			}
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("the retry left it: %v", err)
		}
	})
	t.Run("cannot be removed", func(t *testing.T) {
		dir := make1(t)
		msg := removeScratch(dir, func(string) error { return fmt.Errorf("stuck") }, 0)
		if !strings.Contains(msg, "still failing") || !strings.Contains(msg, "local-base.db-wal") {
			t.Errorf("message:\n%s", msg)
		}
	})
	t.Run("limit and content", func(t *testing.T) {
		dir := make1(t)
		for i := range 60 {
			os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d", i)), []byte("the secret text"), 0o600)
		}
		got := leftovers(dir, time.Now())
		if !strings.Contains(got, "more") || strings.Contains(got, "secret") {
			t.Errorf("listing:\n%s", got)
		}
	})
	t.Run("the helper cleans up after a passing test", func(t *testing.T) {
		var dir string
		t.Run("inner", func(t *testing.T) { dir = scratchDir(t); os.WriteFile(filepath.Join(dir, "x"), nil, 0o600) })
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("scratchDir left %s: %v", dir, err)
		}
	})
}
