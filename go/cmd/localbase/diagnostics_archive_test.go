package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// The archive is published as a new private file, never over one that is there, never through a
// link in its directory's path, and it leaves no temporary file behind. Each refusal has its words.
func TestDiagnosticsArchiveIsPublishedSafely(t *testing.T) {
	entries := []zipEntry{{name: "manifest.json", data: []byte(`{"a":1}`)}, {name: "logs/events.jsonl", data: []byte("x\n")}}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "bundle.zip")
	size, err := writeDiagnosticsArchive(path, entries)
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
		if _, err := writeDiagnosticsArchive(path, entries); err == nil || err.Error() != want {
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
	big := []zipEntry{{name: "big", data: bytes.Repeat([]byte("x"), maxDiagnosticsSource+1)}}
	if _, err := writeDiagnosticsArchive(filepath.Join(dir, "big.zip"), big); err == nil || err.Error() != "Diagnostics archive exceeds the uncompressed size limit." {
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

// The bundle counts the stored custom voices, and says nothing else about them.
func TestDiagnosticsCountsCustomVoices(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("LOCALBASE_ROOT", "")
	root := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	count := func() any {
		t.Helper()
		bundle := filepath.Join(t.TempDir(), "bundle.zip")
		if code, out := runApp(t, "--root", root, "diagnostics", "--output", bundle); code != 0 {
			t.Fatalf("diagnostics: %s", out)
		}
		zr, err := zip.OpenReader(bundle)
		if err != nil {
			t.Fatal(err)
		}
		defer zr.Close()
		for _, f := range zr.File {
			if f.Name == "manifest.json" {
				rc, _ := f.Open()
				var m struct {
					Voices map[string]any `json:"voices"`
				}
				if err := json.NewDecoder(rc).Decode(&m); err != nil {
					t.Fatal(err)
				}
				rc.Close()
				return m.Voices["data"]
			}
		}
		t.Fatal("no manifest")
		return nil
	}
	if got := fmt.Sprint(count()); got != "map[customVoices:0]" {
		t.Errorf("none: %s", got)
	}
	st, err := store.Open(filepath.Join(root, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if _, err := st.DB.Exec(`INSERT INTO tts_custom_voices (id, name, name_key, sha256, size_bytes, duration_ms, created_at, created_by, consent_statement, consent_attested_at)
			VALUES (?, ?, ?, ?, 1000, 5000, '2026-01-01T00:00:00.000Z', 'cli', 'I consent.', '2026-01-01T00:00:00.000Z')`, "voice_"+id, "Name "+id, "name "+id, strings.Repeat(id, 64)); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	if got := fmt.Sprint(count()); got != "map[customVoices:2]" {
		t.Errorf("two: %s", got)
	}
}
