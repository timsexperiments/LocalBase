package safefile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func wantUnsafe(t *testing.T, err error, problem string) {
	t.Helper()
	var unsafe *UnsafeFileError
	if !errors.As(err, &unsafe) || !strings.Contains(unsafe.Problem, problem) {
		t.Errorf("want an unsafe file (%s): %v", problem, err)
	}
}

// victim is a file outside the root that a planted link points at.
func victim(t *testing.T) (string, []byte) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "victim")
	content := []byte("someone's own file\n")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return p, content
}

func victimUntouched(t *testing.T, path string, content []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(content) {
		t.Errorf("the victim changed: %q %v", got, err)
	}
	if m := fileMode(t, path); m != 0o644 {
		t.Errorf("the victim's mode changed: %o", m)
	}
}

func TestOpenOwnedRegular(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	// Created private; a loose existing one comes out private too.
	f, err := OpenOwnedRegular(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if m := fileMode(t, path); m != 0o600 {
		t.Errorf("new: %o", m)
	}
	os.Chmod(path, 0o644)
	f, err = OpenOwnedRegular(path, os.O_WRONLY|os.O_APPEND)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if m := fileMode(t, path); m != 0o600 {
		t.Errorf("tightened: %o", m)
	}
	// Missing, without create.
	if _, err := OpenOwnedRegular(filepath.Join(dir, "none"), os.O_RDONLY); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}

	target, content := victim(t)
	link := filepath.Join(dir, "link")
	os.Symlink(target, link)
	_, err = OpenOwnedRegular(link, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	wantUnsafe(t, err, "symbolic link")
	victimUntouched(t, target, content)
	if !strings.Contains(err.Error(), link) || !strings.Contains(err.Error(), "Remove it") || !strings.Contains(err.Error(), "who can write") {
		t.Errorf("the message should name the file and what to do: %v", err)
	}

	dangling := filepath.Join(dir, "dangling")
	gone := filepath.Join(t.TempDir(), "not-yet")
	os.Symlink(gone, dangling)
	_, err = OpenOwnedRegular(dangling, os.O_CREATE|os.O_WRONLY)
	wantUnsafe(t, err, "symbolic link")
	if _, statErr := os.Stat(gone); statErr == nil {
		t.Error("created the file a dangling link pointed at")
	}

	hard := filepath.Join(dir, "hard")
	other := filepath.Join(dir, "other-name")
	os.WriteFile(hard, []byte("x"), 0o600)
	os.Link(hard, other)
	_, err = OpenOwnedRegular(hard, os.O_WRONLY)
	wantUnsafe(t, err, "hard link")
	for _, want := range []string{"second name points at the same file", "`ln`", "Delete the other name", "`cp`"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the hard-link message lacks %q: %v", want, err)
		}
	}

	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := OpenOwnedRegular(fifo, os.O_WRONLY)
		done <- err
	}()
	select {
	case err := <-done:
		wantUnsafe(t, err, "not a regular file")
	case <-time.After(5 * time.Second):
		t.Fatal("a FIFO hung the open")
	}

	sub := filepath.Join(dir, "sub")
	os.Mkdir(sub, 0o700)
	_, err = OpenOwnedRegular(sub, os.O_RDONLY)
	wantUnsafe(t, err, "not a regular file")

	// Someone else's file. Chowning needs root, so this user is told it isn't them.
	real := os.Getuid()
	someoneElse := func() int { return real + 1 }
	_, err = openOwnedRegular(path, os.O_WRONLY, options{uid: someoneElse})
	wantUnsafe(t, err, "another user")
}

// A file swapped in after the first check, even a plain one, is noticed.
func TestOpenOwnedRegularNoticesASwap(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "log")
	os.WriteFile(path, []byte("x"), 0o600)
	plainSwap := func() {
		os.Remove(path)
		os.WriteFile(path, []byte("y"), 0o600)
	}
	_, err := openOwnedRegular(path, os.O_WRONLY, options{afterOpen: plainSwap})
	wantUnsafe(t, err, "changed")
	linkSwap := func() {
		target := path + ".target"
		os.WriteFile(target, []byte("y"), 0o600)
		os.Remove(path)
		os.Symlink(target, path)
	}
	os.Remove(path)
	os.WriteFile(path, []byte("x"), 0o600)
	_, err = openOwnedRegular(path, os.O_WRONLY, options{afterOpen: linkSwap})
	wantUnsafe(t, err, "symbolic link")
}

// A link planted between the check and the open: only O_NOFOLLOW stops a truncate
// reaching its target.
func TestOpenOwnedRegularLinkInTheWindow(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "log")
	os.WriteFile(path, []byte("x"), 0o600)
	target, content := victim(t)
	plantLink := func() {
		os.Remove(path)
		os.Symlink(target, path)
	}
	_, err := openOwnedRegular(path, os.O_WRONLY|os.O_TRUNC, options{beforeOpen: plantLink})
	wantUnsafe(t, err, "symbolic link")
	victimUntouched(t, target, content)
}

// A plain file swapped in between the check and the open is not the file that was checked.
func TestOpenOwnedRegularPlainSwapBeforeTheOpen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "log")
	os.WriteFile(path, []byte("x"), 0o600)
	plainSwap := func() {
		os.Remove(path)
		os.WriteFile(path, []byte("y"), 0o600)
	}
	_, err := openOwnedRegular(path, os.O_WRONLY, options{beforeOpen: plainSwap})
	wantUnsafe(t, err, "changed")
}

// A FIFO swapped in between the check and the open does not hang it. Opened to read, it is
// refused as not a plain file; opened to write, with no reader, the open itself fails.
func TestOpenOwnedRegularFIFOSwapBeforeTheOpen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "log")
	fifoSwap := func() {
		os.Remove(path)
		syscall.Mkfifo(path, 0o600)
	}
	for _, flags := range []int{os.O_RDONLY, os.O_WRONLY} {
		os.Remove(path)
		os.WriteFile(path, []byte("x"), 0o600)
		done := make(chan error, 1)
		go func() {
			_, err := openOwnedRegular(path, flags, options{beforeOpen: fifoSwap})
			done <- err
		}()
		select {
		case err := <-done:
			if flags == os.O_RDONLY {
				wantUnsafe(t, err, "not a regular file")
			} else if err == nil {
				t.Error("a FIFO swapped in before a write open was opened")
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("a FIFO swapped in before the open hung it (flags %d)", flags)
		}
	}
}

// A second name made for the file after the open is seen on the file that was opened.
func TestOpenOwnedRegularHardLinkAfterTheOpen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "log")
	os.WriteFile(path, []byte("x"), 0o600)
	secondName := func() { os.Link(path, path+".2") }
	_, err := openOwnedRegular(path, os.O_WRONLY, options{afterOpen: secondName})
	wantUnsafe(t, err, "hard link")
}

// The owner is checked three times: the path before the open, the file that was opened, and the
// path after. Another user's file seen by any one of those checks alone is refused there.
func TestOpenOwnedRegularChecksTheOwnerThreeTimes(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "log")
	os.WriteFile(path, []byte("x"), 0o600)
	real := os.Getuid()
	for at := 1; at <= 3; at++ {
		calls := 0
		owner := func() int {
			calls++
			if calls == at {
				return real + 1
			}
			return real
		}
		_, err := openOwnedRegular(path, os.O_WRONLY, options{uid: owner})
		wantUnsafe(t, err, "another user")
		if calls != at {
			t.Errorf("check %d: the owner was asked %d times before the refusal", at, calls)
		}
	}
}

func TestWritePrivateAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := WritePrivateAtomic(path, []byte("one")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "one" {
		t.Errorf("%q", got)
	}
	if m := fileMode(t, path); m != 0o600 {
		t.Errorf("mode: %o", m)
	}
	// A planted fixed temporary name, or a link at the path, redirects nothing.
	target, content := victim(t)
	os.Symlink(target, path+".tmp")
	if err := WritePrivateAtomic(path, []byte("two")); err != nil {
		t.Fatal(err)
	}
	victimUntouched(t, target, content)
	os.Remove(path)
	os.Symlink(target, path)
	if err := WritePrivateAtomic(path, []byte("three")); err != nil {
		t.Fatal(err)
	}
	victimUntouched(t, target, content)
	if info, _ := os.Lstat(path); info.Mode()&fs.ModeSymlink != 0 {
		t.Error("the link is still there")
	}
	if got, _ := os.ReadFile(path); string(got) != "three" {
		t.Errorf("%q", got)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") && e.Name() != "state.json.tmp" {
			t.Errorf("left behind: %s", e.Name())
		}
	}
}
