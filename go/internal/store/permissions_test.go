package store

import (
	"os"
	"path/filepath"
	"testing"
)

func fileMode(t *testing.T, p string) os.FileMode {
	t.Helper()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

// A fresh database is private, and so are the journal files SQLite makes beside
// it, which take its mode. This is what the first-run file would otherwise miss:
// the database holds the Hugging Face token.
func TestDatabaseIsPrivate(t *testing.T) {
	old := setUmask(0o022)
	defer setUmask(old)
	path := filepath.Join(t.TempDir(), "local-base.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if m := fileMode(t, path); m != 0o600 {
		t.Errorf("fresh database: %o", m)
	}
	if _, err := st.DB.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`CREATE TABLE wal_check (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if m := fileMode(t, path+suffix); m != 0o600 {
			t.Errorf("%s: %o", suffix, m)
		}
	}
}

func TestOpenTightensAnExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-base.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.DB.Exec(`PRAGMA journal_mode=WAL`)
	st.DB.Exec(`CREATE TABLE wal_check (id INTEGER)`)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Chmod(path+suffix, 0o644)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	defer st.Close()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if m := fileMode(t, path+suffix); m != 0o600 {
			t.Errorf("%q: %o", suffix, m)
		}
	}
	var n int
	if err := again.DB.QueryRow(`SELECT count(*) FROM wal_check`).Scan(&n); err != nil {
		t.Errorf("the existing data: %v", err)
	}
}

func TestOpenLeavesASymlinkedDatabaseMode(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.db")
	st, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	os.Chmod(target, 0o640)
	link := filepath.Join(dir, "link.db")
	os.Symlink(target, link)
	st, err = Open(link)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if m := fileMode(t, target); m != 0o640 {
		t.Errorf("changed through a symlink: %o", m)
	}
}
