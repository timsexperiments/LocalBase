package safefile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The data reaches the disk before the name does. Otherwise a crash can keep the rename and lose
// the data, and a restore journal or a settings file is then there and empty.
func TestAPrivateWriteReachesTheDiskBeforeItsName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	var synced []string
	watchSync := func(f *os.File) error {
		synced = append(synced, f.Name())
		if b, _ := os.ReadFile(path); string(b) != "old" {
			t.Errorf("the name had the new data before it was synced: %q", b)
		}
		if b, _ := os.ReadFile(f.Name()); string(b) != "new" {
			t.Errorf("what was synced: %q", b)
		}
		return f.Sync()
	}
	if err := writePrivateAtomic(path, []byte("new"), options{sync: watchSync}); err != nil {
		t.Fatal(err)
	}
	if len(synced) != 1 || filepath.Dir(synced[0]) != dir || synced[0] == path {
		t.Fatalf("synced %v, want the one temporary file beside %s", synced, path)
	}
	if b, _ := os.ReadFile(path); string(b) != "new" {
		t.Errorf("after the write: %q", b)
	}
}

// A sync that fails is a write that fails: the old file stays, and nothing is left beside it.
func TestAFailedSyncLeavesTheFileAsItWas(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	failSync := func(*os.File) error { return errors.New("planted sync failure") }
	if err := writePrivateAtomic(path, []byte("new"), options{sync: failSync}); err == nil || !strings.Contains(err.Error(), "planted sync failure") {
		t.Fatalf("got %v, want the sync failure", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "old" {
		t.Errorf("the file changed: %q", b)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("left beside it: %v", entries)
	}
}
