package contract

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// The contract tests' file-system guard. Every create, write, truncate,
// directory creation and removal by the harness and the tests goes through
// it (TestNoUnguardedWrites enforces this for the package), and it refuses:
//
//   - any path whose real location is outside a registered scratch root;
//   - writing through a symlink, or to an existing file with more than one
//     link (a hard link shares its inode with another file);
//   - real model files brought in any way but an APFS clone or a read-only
//     copy (cloneModel): never a symlink or hard link into the live root.
//
// Scratch roots are directories the tests created themselves: the
// harness's per-gateway roots and each test's t.TempDir().

var scratch struct {
	mu    sync.Mutex
	roots []string // real paths
}

// registerScratch makes dir, which the caller created, a scratch root, and
// returns its real path.
func registerScratch(dir string) (string, error) {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	if live := liveRoot(); live != "" && within(real, live) {
		return "", fmt.Errorf("fsguard: %s is inside the live LocalBase root", real)
	}
	scratch.mu.Lock()
	scratch.roots = append(scratch.roots, real)
	scratch.mu.Unlock()
	return real, nil
}

// scratchDir is t.TempDir(), registered as a scratch root.
func scratchDir(t testing.TB) string {
	t.Helper()
	dir, err := registerScratch(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// liveRoot is the installed LocalBase root (~/.local/share/local-base).
func liveRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	live := filepath.Join(home, ".local", "share", "local-base")
	if real, err := filepath.EvalSymlinks(live); err == nil {
		return real
	}
	return live
}

func within(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// realTarget resolves where a write to path would land: the real path of an
// existing entry, or of the nearest existing ancestor plus the rest.
func realTarget(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rest := []string{}
	cur := abs
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(append([]string{real}, rest...)...), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("fsguard: cannot resolve %s", path)
		}
		rest = append([]string{filepath.Base(cur)}, rest...)
		cur = parent
	}
}

// ErrGuard is a write the guard refused.
var ErrGuard = errors.New("fsguard: refused")

// checkWrite refuses a write to path unless it lands inside a scratch root,
// is not a symlink, and is not a file with more than one link.
func checkWrite(path string) error {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s is a symlink", ErrGuard, path)
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && fi.Mode().IsRegular() && st.Nlink > 1 {
			return fmt.Errorf("%w: %s shares its inode (%d links)", ErrGuard, path, st.Nlink)
		}
	}
	real, err := realTarget(path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrGuard, err)
	}
	if live := liveRoot(); live != "" && within(real, live) {
		return fmt.Errorf("%w: %s is inside the live LocalBase root", ErrGuard, real)
	}
	scratch.mu.Lock()
	defer scratch.mu.Unlock()
	for _, root := range scratch.roots {
		if within(real, root) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s is outside every scratch root", ErrGuard, real)
}

// --- guarded operations -------------------------------------------------------------------

func guardedWriteFile(path string, b []byte, perm os.FileMode) error {
	if err := checkWrite(path); err != nil {
		return err
	}
	return os.WriteFile(path, b, perm)
}

func guardedCreate(path string) (*os.File, error) {
	if err := checkWrite(path); err != nil {
		return nil, err
	}
	return os.Create(path)
}

func guardedAppend(path string) (*os.File, error) {
	if err := checkWrite(path); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
}

func guardedMkdirAll(path string, perm os.FileMode) error {
	if err := checkWrite(path); err != nil {
		return err
	}
	return os.MkdirAll(path, perm)
}

func guardedRemoveAll(path string) error {
	if err := checkWrite(path); err != nil {
		return err
	}
	return os.RemoveAll(path)
}

// guardedSparse creates a new file of size bytes (a sparse placeholder).
// An existing entry is left alone.
func guardedSparse(path string, size int64) error {
	if _, err := os.Lstat(path); err == nil {
		return nil
	}
	if err := checkWrite(path); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	err = f.Truncate(size)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// guardedCopyDir copies a directory tree into a scratch root (cp -R).
func guardedCopyDir(src, dst string) error {
	if err := checkWrite(dst); err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	if out, err := exec.Command("cp", "-R", src+"/.", dst).CombinedOutput(); err != nil {
		return fmt.Errorf("copy %s: %v %s", src, err, out)
	}
	return nil
}

// maxReadOnlyCopy is the largest model file copied when a clone is not
// possible; larger ones need an APFS clone.
const maxReadOnlyCopy = 64 << 20

// cloneModel brings a real model file into a scratch root as an APFS clone
// (cp -c, copy-on-write: the source is only read) or, for a small file, a
// read-only copy. Never a symlink or hard link. The result is made
// read-only and checked: a regular file with one link, a different inode
// from the source.
func cloneModel(src, dst string) error {
	if err := checkWrite(dst); err != nil {
		return err
	}
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("%w: %s already exists", ErrGuard, dst)
	}
	sfi, err := os.Stat(src)
	if err != nil {
		return err
	}
	cloned := false
	if goruntime.GOOS == "darwin" {
		cloned = exec.Command("cp", "-c", src, dst).Run() == nil
	}
	if !cloned {
		if sfi.Size() > maxReadOnlyCopy {
			return fmt.Errorf("%w: %s (%d bytes) can't be cloned here and is too large to copy", ErrGuard, src, sfi.Size())
		}
		if err := copyFile(src, dst); err != nil {
			return err
		}
	}
	if err := os.Chmod(dst, 0o400); err != nil {
		return err
	}
	return assertDetached(src, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// assertDetached checks that dst is its own file: not a symlink, one link,
// a different inode from src.
func assertDetached(src, dst string) error {
	fi, err := os.Lstat(dst)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrGuard, dst)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	sfi, serr := os.Stat(src)
	if !ok || serr != nil {
		return fmt.Errorf("%w: cannot inspect %s", ErrGuard, dst)
	}
	if st.Nlink != 1 || os.SameFile(fi, sfi) {
		return fmt.Errorf("%w: %s is linked to %s", ErrGuard, dst, src)
	}
	return nil
}

// --- tests --------------------------------------------------------------------------------

// TestFSGuard tries every forbidden write and shows the guard refuses it.
func TestFSGuard(t *testing.T) {
	root := scratchDir(t)
	outside := t.TempDir() // never registered
	victim := filepath.Join(outside, "victim.bin")
	if err := os.WriteFile(victim, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	refused := func(label string, err error) {
		t.Helper()
		if !errors.Is(err, ErrGuard) {
			t.Errorf("%s: not refused (%v)", label, err)
		}
	}

	// Outside every scratch root.
	refused("write outside", guardedWriteFile(filepath.Join(outside, "x"), []byte("x"), 0o600))
	refused("create outside", func() error { _, err := guardedCreate(filepath.Join(outside, "y")); return err }())
	refused("sparse outside", guardedSparse(filepath.Join(outside, "z"), 10))
	refused("mkdir outside", guardedMkdirAll(filepath.Join(outside, "d"), 0o700))
	refused("remove outside", guardedRemoveAll(victim))
	refused("dot-dot escape", guardedWriteFile(filepath.Join(root, "..", filepath.Base(outside), "victim.bin"), []byte("x"), 0o600))
	// A directory symlink inside the scratch root that points outside it.
	link := filepath.Join(root, "models")
	os.Symlink(outside, link)
	refused("through a directory symlink", guardedWriteFile(filepath.Join(link, "victim.bin"), []byte("x"), 0o600))
	refused("new file under a directory symlink", guardedSparse(filepath.Join(link, "new.bin"), 10))

	// A symlink to a file, even one inside the root.
	inner := filepath.Join(root, "inner.bin")
	os.WriteFile(inner, []byte("inner"), 0o600)
	fileLink := filepath.Join(root, "file-link.bin")
	os.Symlink(victim, fileLink)
	refused("write through a file symlink", guardedWriteFile(fileLink, []byte("x"), 0o600))
	refused("create through a file symlink", func() error { _, err := guardedCreate(fileLink); return err }())
	refused("append through a file symlink", func() error { _, err := guardedAppend(fileLink); return err }())
	innerLink := filepath.Join(root, "inner-link.bin")
	os.Symlink(inner, innerLink)
	refused("symlink to a file in the root", guardedWriteFile(innerLink, []byte("x"), 0o600))

	// A hard link: an existing file with more than one link.
	hard := filepath.Join(root, "hard.bin")
	if err := os.Link(inner, hard); err != nil {
		t.Fatal(err)
	}
	refused("write to a hard link", guardedWriteFile(hard, []byte("x"), 0o600))
	refused("write to the other name of a hard link", guardedWriteFile(inner, []byte("x"), 0o600))
	refused("append to a hard link", func() error { _, err := guardedAppend(hard); return err }())

	// The live root.
	if live := liveRoot(); live != "" {
		refused("write into the live root", checkWrite(filepath.Join(live, "models", "fsguard-probe")))
		if _, err := registerScratch(filepath.Join(live, "models")); err == nil {
			t.Error("the live root was accepted as a scratch root")
		}
	}

	// Clones only: a model already linked in, or an existing destination,
	// is refused; a clone or small copy is detached and read-only.
	refused("clone over an existing entry", cloneModel(victim, fileLink))
	model := filepath.Join(outside, "model.gguf")
	os.WriteFile(model, []byte("weights"), 0o600)
	dst := filepath.Join(root, "model.gguf")
	if err := cloneModel(model, dst); err != nil {
		t.Fatalf("clone: %v", err)
	}
	if fi, _ := os.Stat(dst); fi.Mode().Perm() != 0o400 {
		t.Errorf("clone is %v, want read-only", fi.Mode().Perm())
	}
	refused("write to a read-only clone's link check", assertDetached(model, hard))
	refused("a symlink presented as a clone", assertDetached(victim, fileLink))

	// Nothing outside changed.
	if b, _ := os.ReadFile(victim); string(b) != "precious" {
		t.Fatalf("the victim file changed: %q", b)
	}
	if b, _ := os.ReadFile(model); string(b) != "weights" {
		t.Fatalf("the source model changed: %q", b)
	}
	// Allowed writes still work.
	if err := guardedWriteFile(filepath.Join(root, "ok.txt"), []byte("ok"), 0o600); err != nil {
		t.Errorf("an allowed write was refused: %v", err)
	}
}

// TestNoUnguardedWrites keeps every write in this package behind the guard:
// only fsguard_test.go may call the os write functions directly.
func TestNoUnguardedWrites(t *testing.T) {
	files, _ := filepath.Glob("*_test.go")
	forbidden := []string{"os.WriteFile(", "os.Create(", "os.OpenFile(", "os.Symlink(", "os.Link(", ".Truncate(",
		"os.MkdirAll(", "os.Mkdir(", "os.Remove(", "os.RemoveAll(", "os.Rename(", "os.Chmod(", `"cp"`, "t.TempDir()"}
	for _, f := range files {
		if f == "fsguard_test.go" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			code := line
			if j := strings.Index(code, "//"); j >= 0 {
				code = code[:j]
			}
			for _, call := range forbidden {
				if strings.Contains(code, call) {
					t.Errorf("%s:%d: %s outside the guard: %s", f, i+1, strings.TrimSuffix(call, "("), strings.TrimSpace(line))
				}
			}
		}
	}
}

// The must* forms are for call sites that ignore os errors, as the tests
// did before the guard: a guard refusal still stops the run (a panic fails
// the test, or the whole run in TestMain), so it is never silent.

func mustGuard(err error) error {
	if errors.Is(err, ErrGuard) {
		panic(err)
	}
	return err
}

func mustWriteFile(path string, b []byte, perm os.FileMode) error {
	return mustGuard(guardedWriteFile(path, b, perm))
}

func mustCreate(path string) (*os.File, error) {
	f, err := guardedCreate(path)
	return f, mustGuard(err)
}

func mustAppend(path string) (*os.File, error) {
	f, err := guardedAppend(path)
	return f, mustGuard(err)
}

func mustMkdirAll(path string, perm os.FileMode) error { return mustGuard(guardedMkdirAll(path, perm)) }

func mustRemoveAll(path string) error { return mustGuard(guardedRemoveAll(path)) }

// scratchTemp is os.MkdirTemp under the system temp directory, registered
// as a scratch root.
func scratchTemp(pattern string) (string, error) {
	dir, err := os.MkdirTemp("", pattern)
	if err != nil {
		return "", err
	}
	return registerScratch(dir)
}

// assertNoLinks walks a scratch root after the fixture is built: no symlink
// and no file sharing its inode anywhere in it, so nothing a gateway writes
// there can reach another file.
func assertNoLinks(root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			// Model files are never links. A runtime package may keep its own
			// library links, but only ones that stay inside the root.
			rel, _ := filepath.Rel(root, path)
			if strings.HasPrefix(rel, "models"+string(filepath.Separator)) {
				return fmt.Errorf("%w: a model file is a symlink: %s", ErrGuard, path)
			}
			if real, err := filepath.EvalSymlinks(path); err != nil || !within(real, realRootOf(root)) {
				return fmt.Errorf("%w: %s links outside the fixture", ErrGuard, path)
			}
			return nil
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && fi.Mode().IsRegular() && st.Nlink > 1 {
			return fmt.Errorf("%w: the fixture holds a hard link: %s", ErrGuard, path)
		}
		return nil
	})
}

func realRootOf(root string) string {
	if real, err := filepath.EvalSymlinks(root); err == nil {
		return real
	}
	return root
}
