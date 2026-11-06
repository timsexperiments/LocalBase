package backup

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func placeFixture(t *testing.T) (archive, out string) {
	t.Helper()
	dir := t.TempDir()
	archive = filepath.Join(dir, "stage", "archive")
	if err := os.MkdirAll(filepath.Dir(archive), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, []byte("the archive's bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	out = filepath.Join(dir, "out", "b"+Extension)
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		t.Fatal(err)
	}
	return archive, out
}

// noLinks is a file system that can't link, so placeFile takes its copying path.
func noLinks(t *testing.T) {
	t.Helper()
	old := linkFile
	t.Cleanup(func() { linkFile = old })
	linkFile = func(from, to string) error { return &os.LinkError{Op: "link", Old: from, New: to, Err: syscall.EXDEV} }
}

// Where links can't be made, a chmod that fails is a backup that wasn't written: it says so, and
// leaves no empty file at the name.
func TestAPlacedBackupIsNeverAnEmptyFile(t *testing.T) {
	archive, out := placeFixture(t)
	noLinks(t)
	old := chmodFile
	t.Cleanup(func() { chmodFile = old })
	chmodFile = func(*os.File, os.FileMode) error { return errors.New("planted chmod failure") }
	if err := placeFile(archive, out); err == nil || !strings.Contains(err.Error(), "planted chmod failure") {
		t.Fatalf("got %v, want the chmod failure", err)
	}
	if _, err := os.Lstat(out); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a file was left at the backup's name: %v", err)
	}
}

// The copying path still writes the whole archive, private.
func TestAPlacedBackupIsCopiedWhereLinksCantBeMade(t *testing.T) {
	archive, out := placeFixture(t)
	noLinks(t)
	if err := placeFile(archive, out); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); string(b) != "the archive's bytes" {
		t.Errorf("copied %q", b)
	}
	if info, err := os.Stat(out); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, %v", info.Mode(), err)
	}
}

// A placed backup's name is synced into its directory, linked or copied, so it lasts through a
// power cut once the command says it was written.
func TestAPlacedBackupsNameIsSynced(t *testing.T) {
	for _, links := range []bool{true, false} {
		archive, out := placeFixture(t)
		if !links {
			noLinks(t)
		}
		var synced []string
		old := dirSync
		dirSync = func(d *os.File) error { synced = append(synced, d.Name()); return d.Sync() }
		err := placeFile(archive, out)
		dirSync = old
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, d := range synced {
			found = found || d == filepath.Dir(out)
		}
		if !found {
			t.Errorf("links %v: synced %v, not %s", links, synced, filepath.Dir(out))
		}
	}
}
