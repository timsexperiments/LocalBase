package install

import (
	"os"
	"path/filepath"
	"testing"
)

// The checksum cache's lock database is the file its directory names, whatever the root's path
// holds: SQLite reads a "file:" name as a URI, and an unescaped "?" or "#" would put it elsewhere.
func TestTheCacheLockIsWhereItsDirectoryIs(t *testing.T) {
	for _, name := range []string{"plain", "a?b", "a#b", "a%41b"} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, name)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := withCacheLock(dir, func() error { return nil }); err != nil {
				t.Fatalf("lock: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, ".checksums.json.lock.db")); err != nil {
				t.Errorf("no lock database in %s: %v", dir, err)
			}
			entries, err := os.ReadDir(parent)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != name {
				var names []string
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Errorf("beside %q: %q", name, names)
			}
		})
	}
}
