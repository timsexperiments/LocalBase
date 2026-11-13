package safefile

import (
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"
)

func TestRealDir(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := RealDir(dir); err != nil || got != dir {
		t.Errorf("a real directory: %q, %v", got, err)
	}
	if _, err := RealDir("relative/dir"); !errors.Is(err, ErrNotAbsolute) {
		t.Errorf("relative: %v", err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{link, filepath.Join(link, "sub"), filepath.Join(dir, "missing"), file} {
		if _, err := RealDir(bad); !errors.Is(err, ErrNotRealDir) {
			t.Errorf("%s: %v", bad, err)
		}
	}
	if goruntime.GOOS == "darwin" {
		for in, want := range map[string]string{"/tmp": "/private/tmp", "/var": "/private/var", "/etc": "/private/etc"} {
			if got, err := RealDir(in); err != nil || got != want {
				t.Errorf("%s: %q, %v", in, got, err)
			}
		}
	}
}

func TestPublishNew(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	leftovers := func() []string { l, _ := filepath.Glob(filepath.Join(dir, ".pre-*")); return l }
	if err := PublishNew(dir, "out", ".pre-", []byte("one")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(dir, "out"))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("published: %v, %v", info, err)
	}
	step := func(err error) string {
		var pe *PublishError
		if !errors.As(err, &pe) {
			return "untyped: " + err.Error()
		}
		return pe.Step
	}
	// Never over what is there: the first stays as it was.
	if err := PublishNew(dir, "out", ".pre-", []byte("two")); err == nil || step(err) != "link" || !errors.Is(err, os.ErrExist) {
		t.Errorf("over an existing file: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "out")); string(b) != "one" {
		t.Errorf("the existing file became %q", b)
	}
	if err := PublishNew(filepath.Join(dir, "missing"), "out", ".pre-", nil); err == nil || step(err) != "create" {
		t.Errorf("no directory: %v", err)
	}
	if l := leftovers(); len(l) != 0 {
		t.Errorf("temporary files left: %q", l)
	}
}
