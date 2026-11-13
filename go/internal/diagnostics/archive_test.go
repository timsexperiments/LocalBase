package diagnostics

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

// The archive is published as a new private file, never over one that is there, never through a
// link in its directory's path, and it leaves no temporary file behind. Each refusal has its words.
func TestDiagnosticsArchiveIsPublishedSafely(t *testing.T) {
	entries := []Entry{{Name: "manifest.json", Data: []byte(`{"a":1}`)}, {Name: "logs/events.jsonl", Data: []byte("x\n")}}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "bundle.zip")
	size, err := WriteArchive(path, entries)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 || int(info.Size()) != size {
		t.Fatalf("published: %v, %v (size %d)", info, err, size)
	}
	zr, err := zip.NewReader(mustOpen(t, path), info.Size())
	if err != nil || len(zr.File) != 2 || zr.File[0].Name != "manifest.json" || zr.File[0].Method != zip.Deflate {
		t.Fatalf("archive: %v", err)
	}
	rc, _ := zr.File[1].Open()
	if b, _ := io.ReadAll(rc); string(b) != "x\n" {
		t.Errorf("entry: %q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".localbase-diagnostics-*")); len(left) != 0 {
		t.Errorf("temporary files left: %q", left)
	}

	refused := func(what, path, want string) {
		t.Helper()
		if _, err := WriteArchive(path, entries); err == nil || err.Error() != want {
			t.Errorf("%s: %v, want %q", what, err, want)
		}
		if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".localbase-diagnostics-*")); len(left) != 0 {
			t.Errorf("%s: temporary files left: %q", what, left)
		}
	}
	refused("an existing file", path, "Secure diagnostics archive publication; output already exists failed.")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	refused("a link in the path", filepath.Join(link, "other.zip"), "Secure diagnostics directory open failed.")
	refused("a missing directory", filepath.Join(dir, "missing", "b.zip"), "Secure diagnostics directory open failed.")
	refused("a relative path", filepath.Join("relative", "b.zip"), "Secure output parent is invalid.")
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		refused("an unwritable directory", filepath.Join(ro, "b.zip"), "Secure diagnostics temporary file creation failed.")
	}
	big := []Entry{{Name: "big", Data: bytes.Repeat([]byte("x"), maxDiagnosticsSource+1)}}
	if _, err := WriteArchive(filepath.Join(dir, "big.zip"), big); err == nil || err.Error() != "Diagnostics archive exceeds the uncompressed size limit." {
		t.Errorf("too big: %v", err)
	}
	// macOS's /tmp is /private/tmp: the alias is read as its target, not refused as a link.
	if goruntime.GOOS == "darwin" {
		if got, err := secureParent("/tmp"); err != nil || got != "/private/tmp" {
			t.Errorf("/tmp: %q, %v", got, err)
		}
	}
	if got, err := secureParent(dir + "/"); err != nil || !strings.HasSuffix(got, filepath.Base(dir)) {
		t.Errorf("dir: %q, %v", got, err)
	}
}

func mustOpen(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}
