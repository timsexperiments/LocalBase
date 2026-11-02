package obs

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

func perm(t *testing.T, path string) os.FileMode {
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
	if m := perm(t, path); m != 0o644 {
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
	if m := perm(t, path); m != 0o600 {
		t.Errorf("new: %o", m)
	}
	os.Chmod(path, 0o644)
	f, err = OpenOwnedRegular(path, os.O_WRONLY|os.O_APPEND)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if m := perm(t, path); m != 0o600 {
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
	defer func(f func() int) { currentUID = f }(currentUID)
	real := currentUID()
	currentUID = func() int { return real + 1 }
	_, err = OpenOwnedRegular(path, os.O_WRONLY)
	wantUnsafe(t, err, "another user")
}

// A file swapped in after the first check, even a plain one, is noticed.
func TestOpenOwnedRegularNoticesASwap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	os.WriteFile(path, []byte("x"), 0o600)
	defer func() { afterSecureOpen = func() {} }()
	afterSecureOpen = func() {
		os.Remove(path)
		os.WriteFile(path, []byte("y"), 0o600)
	}
	_, err := OpenOwnedRegular(path, os.O_WRONLY)
	wantUnsafe(t, err, "changed")
	afterSecureOpen = func() {
		target := path + ".target"
		os.WriteFile(target, []byte("y"), 0o600)
		os.Remove(path)
		os.Symlink(target, path)
	}
	os.Remove(path)
	os.WriteFile(path, []byte("x"), 0o600)
	_, err = OpenOwnedRegular(path, os.O_WRONLY)
	wantUnsafe(t, err, "symbolic link")
}

// A link planted between the check and the open: only O_NOFOLLOW stops a truncate
// reaching its target.
func TestOpenOwnedRegularLinkInTheWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	os.WriteFile(path, []byte("x"), 0o600)
	target, content := victim(t)
	defer func() { beforeSecureOpen = func() {} }()
	beforeSecureOpen = func() {
		os.Remove(path)
		os.Symlink(target, path)
	}
	_, err := OpenOwnedRegular(path, os.O_WRONLY|os.O_TRUNC)
	wantUnsafe(t, err, "symbolic link")
	victimUntouched(t, target, content)
}

func TestSinkRefusesAPlantedLink(t *testing.T) {
	dir := t.TempDir()
	target, content := victim(t)
	os.Symlink(target, filepath.Join(dir, "events.jsonl"))
	_, err := OpenSink(dir)
	wantUnsafe(t, err, "symbolic link")
	victimUntouched(t, target, content)
}

// Rotated files stay private, and a link planted between rotations stops the sink
// from writing through it.
func TestRotatedFilesStayPrivate(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSink(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Write([]byte("first\n"))
	s.size = rotateBytes
	s.Write([]byte("second\n"))
	for _, name := range []string{"events.jsonl", "events.1.jsonl"} {
		if m := perm(t, filepath.Join(dir, name)); m != 0o600 {
			t.Errorf("%s: %o", name, m)
		}
	}
	target, content := victim(t)
	s.size = rotateBytes
	// Rotating renames the active path and opens it again; a link at that next open
	// must be refused. Plant one in the archive's place too: it is renamed, not followed.
	os.Symlink(target, filepath.Join(dir, "events.5.jsonl"))
	s.Write([]byte("third\n"))
	s.size = rotateBytes
	real := afterSecureOpen
	defer func() { afterSecureOpen = real }()
	os.Rename(s.path(), s.path()+".moved")
	os.Symlink(target, s.path())
	s.Write([]byte("fourth\n"))
	victimUntouched(t, target, content)
}

func TestBoundServiceStderrRefusesALink(t *testing.T) {
	root := t.TempDir()
	active, archive := stderrPaths(root)
	os.MkdirAll(filepath.Dir(active), 0o700)
	target := filepath.Join(t.TempDir(), "victim")
	big := strings.Repeat("x", maxServiceStderr+5)
	os.WriteFile(target, []byte(big), 0o644)
	os.Symlink(target, active)
	wantUnsafe(t, BoundServiceStderr(root), "symbolic link")
	if got, _ := os.ReadFile(target); len(got) != len(big) {
		t.Errorf("the target was truncated: %d bytes", len(got))
	}
	if _, err := os.Stat(archive); err == nil {
		t.Error("its contents were copied into the archive")
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
	if m := perm(t, path); m != 0o600 {
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

func TestCheckRootFiles(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"logs", "runs", "runtime"} {
		os.Mkdir(filepath.Join(root, d), 0o700)
	}
	for _, f := range []string{"logs/events.jsonl", "logs/events.1.jsonl", "logs/service-stderr.log", "runs/run_a.json", "runtime/model-operations.json"} {
		os.WriteFile(filepath.Join(root, f), []byte("x"), 0o600)
	}
	if got := CheckRootFiles(root); len(got) != 0 {
		t.Errorf("a normal root: %v", got)
	}
	if got := CheckRootFiles(t.TempDir()); len(got) != 0 {
		t.Errorf("an empty root: %v", got)
	}
	target, content := victim(t)
	for _, rel := range []string{"logs/events.jsonl", "logs/events.2.jsonl", "logs/service-stderr.log", "logs/service-stderr.log.1", "runs/run_a.json", "runtime/model-operations.json"} {
		p := filepath.Join(root, rel)
		os.Remove(p)
		os.Symlink(target, p)
	}
	got := CheckRootFiles(root)
	if len(got) != 6 {
		t.Errorf("found %d: %v", len(got), got)
	}
	for _, e := range got {
		if !strings.Contains(e.Problem, "symbolic link") {
			t.Errorf("%v", e)
		}
	}
	victimUntouched(t, target, content)
	// A linked directory is one problem, and nothing is read through it.
	root2 := t.TempDir()
	os.Symlink(t.TempDir(), filepath.Join(root2, "logs"))
	if got := CheckRootFiles(root2); len(got) != 1 || !strings.HasSuffix(got[0].Path, "logs") {
		t.Errorf("a linked directory: %v", got)
	}
}
