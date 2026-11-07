package store

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// A root's path is the user's to choose, and SQLite reads a "file:" name as a URI: unescaped, a "?"
// or "#" in it ends the file name, and "%41" is read as "A". Then SQLite opens one file while the
// store guards and holds another. So the database must be the file the path names, and nothing
// else may appear beside it.
var awkwardNames = []string{"plain", "a?b", "a#b", "a%41b", "a b"}

func entriesOf(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func TestTheDatabaseIsTheFileItsPathNames(t *testing.T) {
	for _, name := range awkwardNames {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, name)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "local-base.db")
			st, err := Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if err := st.SetSetting(t.Context(), "dsn-test", "here"); err != nil {
				t.Errorf("write: %v", err)
			}
			// With the store still open its -shm is there, so a read-only open reads the file itself
			// rather than a private copy.
			if _, err := os.Stat(path + "-shm"); err != nil {
				t.Fatalf("no -shm beside an open store: %v", err)
			}
			live, err := OpenReadOnly(path)
			if err != nil {
				t.Fatalf("open read-only beside the store: %v", err)
			}
			v, ok, err := live.Setting(t.Context(), "dsn-test")
			live.Close()
			if err != nil || !ok || v != "here" {
				t.Errorf("read back beside the store %q, %v, %v: the read-only open didn't read the same file", v, ok, err)
			}
			if err := st.Close(); err != nil {
				t.Errorf("close: %v", err)
			}
			if got := entriesOf(t, parent); len(got) != 1 || got[0] != name {
				t.Errorf("beside the root's directory: %q, want only %q", got, name)
			}
			info, err := os.Stat(path)
			if err != nil || info.Size() == 0 {
				t.Fatalf("the database isn't at %s: %v", path, err)
			}

			ro, err := OpenReadOnly(path)
			if err != nil {
				t.Fatalf("open read-only: %v", err)
			}
			v, ok, err = ro.Setting(t.Context(), "dsn-test")
			ro.Close()
			if err != nil || !ok || v != "here" {
				t.Errorf("read back %q, %v, %v: the read-only open didn't read the same file", v, ok, err)
			}
			if got := entriesOf(t, parent); len(got) != 1 || got[0] != name {
				t.Errorf("after the read-only open, beside the root's directory: %q", got)
			}
		})
	}
}
