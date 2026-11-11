package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeChrome is a directory as tools/e2e.sh leaves it: chrome.zip with what it holds beside it.
func fakeChrome(t *testing.T) (dir, archive, sum string) {
	t.Helper()
	dir = t.TempDir()
	archive = filepath.Join(dir, "chrome.zip")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	for name, body := range map[string]string{
		"chrome/chrome":        strings.Repeat("the browser ", 1000),
		"chrome/lib/thing.pak": "resources",
		"chrome/empty":         "",
	} {
		e, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h := &zip.FileHeader{Name: "chrome/current"}
	h.SetMode(os.ModeSymlink | 0o777)
	e, err := w.CreateHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Write([]byte("lib")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("lib", filepath.Join(dir, "chrome", "current")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if sum, err = sha256File(archive); err != nil {
		t.Fatal(err)
	}
	return dir, archive, sum
}

func TestAWholeChromePasses(t *testing.T) {
	dir, archive, sum := fakeChrome(t)
	n, err := verifyChrome(dir, archive, sum)
	if err != nil || n != 4 {
		t.Errorf("%d files, %v", n, err)
	}
}

// A half-written or changed file in the cache is rejected, not used.
func TestABadChromeIsRejected(t *testing.T) {
	for _, c := range []struct {
		name, want string
		spoil      func(t *testing.T, dir, archive string)
	}{
		{"a half-written archive", "checksum", func(t *testing.T, _, archive string) { halve(t, archive) }},
		{"a half-written browser", "bytes, and the archive says", func(t *testing.T, dir, _ string) { halve(t, filepath.Join(dir, "chrome", "chrome")) }},
		{"a changed browser, the same size", "differs from the archive", func(t *testing.T, dir, _ string) {
			path := filepath.Join(dir, "chrome", "chrome")
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			b[len(b)-1] ^= 1
			if err := os.WriteFile(path, b, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"a missing file", "thing.pak", func(t *testing.T, dir, _ string) {
			if err := os.Remove(filepath.Join(dir, "chrome", "lib", "thing.pak")); err != nil {
				t.Fatal(err)
			}
		}},
		{"a file the archive does not hold", "not in the archive", func(t *testing.T, dir, _ string) {
			if err := os.WriteFile(filepath.Join(dir, "chrome", "added"), []byte("x"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"a symlink pointing elsewhere", "links to", func(t *testing.T, dir, _ string) {
			path := filepath.Join(dir, "chrome", "current")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("elsewhere", path); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, archive, sum := fakeChrome(t)
			c.spoil(t, dir, archive)
			_, err := verifyChrome(dir, archive, sum)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want it to say %q", err, c.want)
			}
		})
	}
}

func halve(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, info.Size()/2); err != nil {
		t.Fatal(err)
	}
}

func fakeAxe(t *testing.T) (script, archive, sum string) {
	t.Helper()
	dir := t.TempDir()
	body := []byte(strings.Repeat("axe.run();", 1000))
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "package/README.md", Mode: 0o644, Size: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "package/axe.min.js", Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	archive, script = filepath.Join(dir, "axe.tgz"), filepath.Join(dir, "axe.min.js")
	if err := os.WriteFile(archive, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, body, 0o644); err != nil {
		t.Fatal(err)
	}
	var err error
	if sum, err = sha256File(archive); err != nil {
		t.Fatal(err)
	}
	return script, archive, sum
}

func TestAxeIsCheckedEveryRun(t *testing.T) {
	script, archive, sum := fakeAxe(t)
	if err := verifyAxe(script, archive, sum); err != nil {
		t.Errorf("a whole axe was refused: %v", err)
	}
	halve(t, script)
	if err := verifyAxe(script, archive, sum); err == nil || !strings.Contains(err.Error(), "differs from the archive") {
		t.Errorf("a half-written script: %v", err)
	}
	script, archive, sum = fakeAxe(t)
	halve(t, archive)
	if err := verifyAxe(script, archive, sum); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("a half-written tarball: %v", err)
	}
}
