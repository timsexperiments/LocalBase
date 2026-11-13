package install

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
)

// countHashes is a hash for verifyWith and Options that counts the full hashes it makes.
func countHashes() (*int, func(string) (string, error)) {
	n := new(int)
	return n, func(path string) (string, error) {
		*n++
		return SHA256File(path)
	}
}

// installWith is installTo with test Options added.
func installWith(t *testing.T, dir string, s *catalog.Spec, o Options) error {
	t.Helper()
	o.Available, o.AllowPrivate = plenty, true
	_, err := Model(context.Background(), s, dir, o)
	return err
}

// One hash for a download, and none for the Verify that starts a video runtime.
func TestNoSecondHashAfterPublish(t *testing.T) {
	srv := newArtifactServer(t, map[string][]byte{"m.bin": modelBytes})
	dir := t.TempDir()
	a := artifactOf("m.bin", modelBytes)
	hashes, hash := countHashes()
	if err := installWith(t, dir, specWith(srv.URL, a), Options{hash: hash}); err != nil {
		t.Fatal(err)
	}
	if *hashes != 1 {
		t.Errorf("the install hashed %d times", *hashes)
	}
	*hashes = 0
	how, err := verifyWith(filepath.Join(dir, "m.bin"), a, dir, hash)
	if err != nil || how != "cached-identity" || *hashes != 0 {
		t.Errorf("Verify after publish: %q %v, %d hashes", how, err, *hashes)
	}
}

// A complete partial left by an earlier run publishes the same way.
func TestNoSecondHashForACompletePartial(t *testing.T) {
	srv := newArtifactServer(t, map[string][]byte{"m.bin": modelBytes})
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "m.bin.partial"), modelBytes, 0o600)
	a := artifactOf("m.bin", modelBytes)
	if err := installTo(t, dir, specWith(srv.URL, a), nil); err != nil {
		t.Fatal(err)
	}
	hashes, hash := countHashes()
	if how, err := verifyWith(filepath.Join(dir, "m.bin"), a, dir, hash); err != nil || how != "cached-identity" || *hashes != 0 {
		t.Errorf("%q %v, %d hashes", how, err, *hashes)
	}
}

// The check stays as strict: a rewrite in place that keeps the size, inode and mtime
// is still caught, because ctime is still in the identity.
func TestRewriteInPlaceIsStillCaught(t *testing.T) {
	srv := newArtifactServer(t, map[string][]byte{"m.bin": modelBytes})
	dir := t.TempDir()
	a := artifactOf("m.bin", modelBytes)
	if err := installTo(t, dir, specWith(srv.URL, a), nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "m.bin")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // past the clock's grain
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteAt([]byte{modelBytes[0] ^ 1}, 0)
	f.Close()
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !os.SameFile(before, after) || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("the test did not keep the inode, size and mtime")
	}
	hashes, hash := countHashes()
	if how, err := verifyWith(path, a, dir, hash); err == nil {
		t.Errorf("a rewritten file passed: %q", how)
	}
	if *hashes != 1 {
		t.Errorf("%d hashes", *hashes)
	}
}

// Whatever goes wrong between the rename and the record costs one extra hash, not a
// failed install, and never records a file that was not the one hashed.
func TestPublishWindowDegradesToOneHash(t *testing.T) {
	a := artifactOf("m.bin", modelBytes)
	t.Run("the file changed in the window", func(t *testing.T) {
		srv := newArtifactServer(t, map[string][]byte{"m.bin": modelBytes})
		dir := t.TempDir()
		path := filepath.Join(dir, "m.bin")
		changed := func() {
			later := time.Now().Add(time.Hour)
			os.Chtimes(path, later, later)
		}
		if err := installWith(t, dir, specWith(srv.URL, a), Options{beforeRecord: changed}); err != nil {
			t.Fatalf("the install should still succeed: %v", err)
		}
		hashes, hash := countHashes()
		if how, err := verifyWith(path, a, dir, hash); err != nil || how != "sha256" || *hashes != 1 {
			t.Errorf("first: %q %v, %d hashes", how, err, *hashes)
		}
		*hashes = 0
		if how, err := verifyWith(path, a, dir, hash); err != nil || how != "cached-identity" || *hashes != 0 {
			t.Errorf("then: %q %v, %d hashes", how, err, *hashes)
		}
	})
	t.Run("the record cannot be written", func(t *testing.T) {
		srv := newArtifactServer(t, map[string][]byte{"m.bin": modelBytes})
		dir := t.TempDir()
		defer os.Chmod(dir, 0o700)
		readOnly := func() { os.Chmod(dir, 0o500) }
		if err := installWith(t, dir, specWith(srv.URL, a), Options{beforeRecord: readOnly}); err != nil {
			t.Fatalf("the install should still succeed: %v", err)
		}
		os.Chmod(dir, 0o700)
		hashes, hash := countHashes()
		if how, err := verifyWith(filepath.Join(dir, "m.bin"), a, dir, hash); err != nil || how != "sha256" || *hashes != 1 {
			t.Errorf("%q %v, %d hashes", how, err, *hashes)
		}
	})
	t.Run("the file was replaced in the window", func(t *testing.T) {
		srv := newArtifactServer(t, map[string][]byte{"m.bin": modelBytes})
		dir := t.TempDir()
		path := filepath.Join(dir, "m.bin")
		replaced := func() {
			bad := append([]byte{}, modelBytes...)
			bad[0] ^= 1
			os.Remove(path)
			os.WriteFile(path, bad, 0o600)
		}
		if err := installWith(t, dir, specWith(srv.URL, a), Options{beforeRecord: replaced}); err != nil {
			t.Fatalf("the install: %v", err)
		}
		if how, err := Verify(path, a, dir); err == nil {
			t.Errorf("a replaced file was trusted: %q", how)
		}
	})
}
