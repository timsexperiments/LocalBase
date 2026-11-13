package lease

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// syncedBeforeRename is a copy of root's (internal/root/synced_write_test.go). Its options record each file synced,
// and check that its final name doesn't hold the new data yet when it is.
func syncedBeforeRename(t *testing.T, final string, want string) (options, *[]string) {
	t.Helper()
	var synced []string
	return options{sync: func(f *os.File) error {
		synced = append(synced, f.Name())
		if b, err := os.ReadFile(final); err == nil && strings.Contains(string(b), want) {
			t.Errorf("%s had the new data before it was synced", final)
		}
		return f.Sync()
	}}, &synced
}

// An operation lease's record is synced before it gets its name, and a failed sync leaves the old one.
func TestALeaseRecordIsSyncedBeforeItsName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner.json")
	if err := os.WriteFile(path, []byte(`{"old":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	o, synced := syncedBeforeRename(t, path, `"new"`)
	if err := writeJSONAtomic(path, map[string]bool{"new": true}, o); err != nil {
		t.Fatal(err)
	}
	if len(*synced) != 1 || (*synced)[0] == path {
		t.Fatalf("synced %v, want the record's temporary file", *synced)
	}

	failing := options{sync: func(*os.File) error { return errors.New("planted sync failure") }}
	if err := writeJSONAtomic(path, map[string]bool{"newer": true}, failing); err == nil || !strings.Contains(err.Error(), "planted sync failure") {
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
