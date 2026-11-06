package root

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// syncedBeforeRename records each file synced, and checks that its final name doesn't hold the new
// data yet when it is.
func syncedBeforeRename(t *testing.T, final string, want string) *[]string {
	t.Helper()
	var synced []string
	old := syncFile
	t.Cleanup(func() { syncFile = old })
	syncFile = func(f *os.File) error {
		synced = append(synced, f.Name())
		if b, err := os.ReadFile(final); err == nil && strings.Contains(string(b), want) {
			t.Errorf("%s had the new data before it was synced", final)
		}
		return f.Sync()
	}
	return &synced
}

// The root's marker is synced before it gets its name, and a failed sync is a failed Init.
func TestTheMarkerIsSyncedBeforeItsName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "root")
	marker := filepath.Join(dir, markerName)
	synced := syncedBeforeRename(t, marker, `"identity"`)
	if _, err := Init(dir); err != nil {
		t.Fatal(err)
	}
	// Init works on the canonical path, which on macOS is under /private.
	if len(*synced) != 1 || filepath.Base((*synced)[0]) != markerName+".tmp" {
		t.Fatalf("synced %v, want the marker's temporary file", *synced)
	}

	failing := filepath.Join(t.TempDir(), "root")
	syncFile = func(*os.File) error { return errors.New("planted sync failure") }
	if _, err := Init(failing); err == nil || !strings.Contains(err.Error(), "planted sync failure") {
		t.Fatalf("got %v, want the sync failure", err)
	}
	if _, err := os.Lstat(filepath.Join(failing, markerName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a marker was left after a failed sync: %v", err)
	}
}

// An operation lease's record is synced before it gets its name, and a failed sync leaves the old one.
func TestALeaseRecordIsSyncedBeforeItsName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner.json")
	if err := os.WriteFile(path, []byte(`{"old":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	synced := syncedBeforeRename(t, path, `"new"`)
	if err := writeJSONAtomic(path, map[string]bool{"new": true}); err != nil {
		t.Fatal(err)
	}
	if len(*synced) != 1 || (*synced)[0] == path {
		t.Fatalf("synced %v, want the record's temporary file", *synced)
	}

	syncFile = func(*os.File) error { return errors.New("planted sync failure") }
	if err := writeJSONAtomic(path, map[string]bool{"newer": true}); err == nil || !strings.Contains(err.Error(), "planted sync failure") {
		t.Fatalf("got %v, want the sync failure", err)
	}
	if b, _ := os.ReadFile(path); string(b) != `{"new":true}` {
		t.Errorf("the record changed: %s", b)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("left beside it: %v", entries)
	}
}
