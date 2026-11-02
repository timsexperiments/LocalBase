package obs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWritePrivateChecked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	for _, content := range []string{"one", "two"} {
		if err := WritePrivateChecked(path, []byte(content)); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(path); string(got) != content {
			t.Errorf("%q", got)
		}
	}
	if m := perm(t, path); m != 0o600 {
		t.Errorf("mode: %o", m)
	}
	// A loose file is rewritten private.
	os.Chmod(path, 0o644)
	if err := WritePrivateChecked(path, []byte("three")); err != nil {
		t.Fatal(err)
	}
	if m := perm(t, path); m != 0o600 {
		t.Errorf("after a loose file: %o", m)
	}
	// A link at the path fails the write, stays in place, and its target is untouched.
	target, content := victim(t)
	link := filepath.Join(dir, "linked.json")
	os.Symlink(target, link)
	err := WritePrivateChecked(link, []byte("planted"))
	wantUnsafe(t, err, "symbolic link")
	victimUntouched(t, target, content)
	if info, _ := os.Lstat(link); info.Mode()&os.ModeSymlink == 0 {
		t.Error("the link was replaced, hiding the problem")
	}
	// A link at the old fixed temporary name redirects nothing.
	os.Symlink(target, path+".tmp")
	if err := WritePrivateChecked(path, []byte("four")); err != nil {
		t.Fatal(err)
	}
	victimUntouched(t, target, content)
	// Other unsafe kinds fail too.
	hard := filepath.Join(dir, "hard.json")
	os.WriteFile(hard, []byte("x"), 0o600)
	os.Link(hard, hard+".other")
	wantUnsafe(t, WritePrivateChecked(hard, []byte("y")), "hard link")
	sub := filepath.Join(dir, "sub")
	os.Mkdir(sub, 0o700)
	var unsafe *UnsafeFileError
	if err := WritePrivateChecked(sub, []byte("y")); !errors.As(err, &unsafe) {
		t.Errorf("a directory: %v", err)
	}
}
