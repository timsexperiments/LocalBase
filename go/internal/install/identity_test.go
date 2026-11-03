package install

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// countHashes counts full hashes for the rest of the test.
func countHashes(t *testing.T) *int {
	t.Helper()
	n := new(int)
	real := hashFile
	hashFile = func(path string) (string, error) {
		*n++
		return real(path)
	}
	t.Cleanup(func() { hashFile = real })
	return n
}

// One hash for a download, and none for the Verify that starts a video runtime.
func TestNoSecondHashAfterPublish(t *testing.T) {
	srv := newArtifactServer(t, map[string][]byte{"m.bin": modelBytes})
	dir := t.TempDir()
	a := artifactOf("m.bin", modelBytes)
	hashes := countHashes(t)
	if err := installTo(t, dir, specWith(srv.URL, a), nil); err != nil {
		t.Fatal(err)
	}
	if *hashes != 1 {
		t.Errorf("the install hashed %d times", *hashes)
	}
	*hashes = 0
	how, err := Verify(filepath.Join(dir, "m.bin"), a, dir)
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
	hashes := countHashes(t)
	if how, err := Verify(filepath.Join(dir, "m.bin"), a, dir); err != nil || how != "cached-identity" || *hashes != 0 {
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
	hashes := countHashes(t)
	if how, err := Verify(path, a, dir); err == nil {
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
	restore := func() { beforeRecord = func() {} }
	defer restore()

	t.Run("the file changed in the window", func(t *testing.T) {
		srv := newArtifactServer(t, map[string][]byte{"m.bin": modelBytes})
		dir := t.TempDir()
		path := filepath.Join(dir, "m.bin")
		beforeRecord = func() {
			later := time.Now().Add(time.Hour)
			os.Chtimes(path, later, later)
		}
		defer restore()
		if err := installTo(t, dir, specWith(srv.URL, a), nil); err != nil {
			t.Fatalf("the install should still succeed: %v", err)
		}
		beforeRecord = func() {}
		hashes := countHashes(t)
		if how, err := Verify(path, a, dir); err != nil || how != "sha256" || *hashes != 1 {
			t.Errorf("first: %q %v, %d hashes", how, err, *hashes)
		}
		*hashes = 0
		if how, err := Verify(path, a, dir); err != nil || how != "cached-identity" || *hashes != 0 {
			t.Errorf("then: %q %v, %d hashes", how, err, *hashes)
		}
	})
	t.Run("the record cannot be written", func(t *testing.T) {
		srv := newArtifactServer(t, map[string][]byte{"m.bin": modelBytes})
		dir := t.TempDir()
		beforeRecord = func() { os.Chmod(dir, 0o500) }
		defer os.Chmod(dir, 0o700)
		if err := installTo(t, dir, specWith(srv.URL, a), nil); err != nil {
			t.Fatalf("the install should still succeed: %v", err)
		}
		os.Chmod(dir, 0o700)
		beforeRecord = func() {}
		hashes := countHashes(t)
		if how, err := Verify(filepath.Join(dir, "m.bin"), a, dir); err != nil || how != "sha256" || *hashes != 1 {
			t.Errorf("%q %v, %d hashes", how, err, *hashes)
		}
	})
	t.Run("the file was replaced in the window", func(t *testing.T) {
		srv := newArtifactServer(t, map[string][]byte{"m.bin": modelBytes})
		dir := t.TempDir()
		path := filepath.Join(dir, "m.bin")
		beforeRecord = func() {
			bad := append([]byte{}, modelBytes...)
			bad[0] ^= 1
			os.Remove(path)
			os.WriteFile(path, bad, 0o600)
		}
		defer restore()
		if err := installTo(t, dir, specWith(srv.URL, a), nil); err != nil {
			t.Fatalf("the install: %v", err)
		}
		beforeRecord = func() {}
		if how, err := Verify(path, a, dir); err == nil {
			t.Errorf("a replaced file was trusted: %q", how)
		}
	})
}
