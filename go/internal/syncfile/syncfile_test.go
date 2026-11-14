package syncfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Write writes the data, private, and syncs the file it wrote once, after the data is in it.
func TestWriteSyncsWhatItWrote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.tmp")
	var synced []string
	sync := func(f *os.File) error {
		b, err := os.ReadFile(f.Name())
		if err != nil || string(b) != "data" {
			t.Errorf("synced %s holding %q (%v), want the data in it", f.Name(), b, err)
		}
		synced = append(synced, f.Name())
		return f.Sync()
	}
	if err := Write(path, []byte("data"), sync); err != nil {
		t.Fatal(err)
	}
	if len(synced) != 1 || synced[0] != path {
		t.Errorf("synced %v, want %s once", synced, path)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, %v", info.Mode(), err)
	}
	// nil is the real sync.
	if err := Write(path, []byte("again"), nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "again" {
		t.Errorf("wrote %q", b)
	}
}

// A sync that fails is Write's error.
func TestWriteSaysItsSyncFailed(t *testing.T) {
	planted := errors.New("planted sync failure")
	if err := Write(filepath.Join(t.TempDir(), "f.tmp"), []byte("data"), func(*os.File) error { return planted }); !errors.Is(err, planted) {
		t.Fatalf("got %v, want the sync's failure", err)
	}
}
