// Package fsguard is the write guard of the black-box suites and their harness
// (go/test/harness). It keeps one list of scratch roots for the whole test binary, so a gateway
// root the harness registers is writable by the tests that use it.
//
// The plain-named writes (WriteFile, Create, Append, OpenRW, MkdirAll, Chmod, Symlink,
// RemoveLink, RemoveAll, Grow) panic on a guard refusal and return any other error, as the
// suites' must* forms did. Sparse, CopyDir, CloneModel, AssertNoLinks and WriteGolden return a
// refusal as an error wrapping ErrGuard and never panic.
package fsguard

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
//     copy (CloneModel): never a symlink or hard link into the live root.
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

// ScratchDir fails the test, without a panic, when the directory can't be registered.
// ScratchDir is t.TempDir(), registered as a scratch root.
func ScratchDir(t testing.TB) string {
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

// guardedOpenRW opens an existing file to read and write in place, such as a database's -shm.
func guardedOpenRW(path string) (*os.File, error) {
	if err := checkWrite(path); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_RDWR, 0)
}

func guardedMkdirAll(path string, perm os.FileMode) error {
	if err := checkWrite(path); err != nil {
		return err
	}
	return os.MkdirAll(path, perm)
}

func guardedChmod(path string, perm os.FileMode) error {
	if err := checkWrite(path); err != nil {
		return err
	}
	return os.Chmod(path, perm)
}

func guardedRemoveAll(path string) error {
	if err := checkWrite(path); err != nil {
		return err
	}
	return os.RemoveAll(path)
}

// Sparse returns a guard refusal as an error wrapping ErrGuard, and never panics.
// Sparse creates a new file of size bytes (a sparse placeholder).
// An existing entry is left alone.
func Sparse(path string, size int64) error {
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

// guardedGrow makes a file at least size bytes long, sparse, creating it if it's missing. Unlike
// Sparse, it also grows a file that's already there.
func guardedGrow(path string, size int64) error {
	if err := checkWrite(path); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if info, statErr := f.Stat(); statErr == nil && info.Size() < size {
		err = f.Truncate(size)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// CopyDir returns a guard refusal as an error wrapping ErrGuard, and never panics.
// CopyDir copies a directory tree into a scratch root (cp -R).
func CopyDir(src, dst string) error {
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

// CloneModel returns a guard refusal as an error wrapping ErrGuard, and never panics.
// CloneModel brings a real model file into a scratch root as an APFS clone
// (cp -c, copy-on-write: the source is only read) or, for a small file, a
// read-only copy. Never a symlink or hard link. The result is made
// read-only and checked: a regular file with one link, a different inode
// from the source.
func CloneModel(src, dst string) error {
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

// The must* forms are for call sites that ignore os errors, as the tests
// did before the guard: a guard refusal still stops the run (a panic fails
// the test, or the whole run in TestMain), so it is never silent.

func mustGuard(err error) error {
	if errors.Is(err, ErrGuard) {
		panic(err)
	}
	return err
}

// WriteFile panics on a guard refusal and returns any other error. It is os.WriteFile behind the guard.
func WriteFile(path string, b []byte, perm os.FileMode) error {
	return mustGuard(guardedWriteFile(path, b, perm))
}

// Create panics on a guard refusal and returns any other error. It is os.Create behind the guard.
func Create(path string) (*os.File, error) {
	f, err := guardedCreate(path)
	return f, mustGuard(err)
}

// Append panics on a guard refusal and returns any other error. It opens an existing file to append, behind the guard.
func Append(path string) (*os.File, error) {
	f, err := guardedAppend(path)
	return f, mustGuard(err)
}

// OpenRW panics on a guard refusal and returns any other error. It opens an existing file to read and write in place, behind the guard.
func OpenRW(path string) (*os.File, error) {
	f, err := guardedOpenRW(path)
	return f, mustGuard(err)
}

// MkdirAll panics on a guard refusal and returns any other error. It is os.MkdirAll behind the guard.
func MkdirAll(path string, perm os.FileMode) error { return mustGuard(guardedMkdirAll(path, perm)) }

// Chmod panics on a guard refusal and returns any other error. It is os.Chmod behind the guard.
func Chmod(path string, perm os.FileMode) error { return mustGuard(guardedChmod(path, perm)) }

// Symlink panics on a guard refusal and returns any other error.
// Symlink plants a link in a scratch directory. Its target may be elsewhere.
func Symlink(target, link string) error {
	if err := mustGuard(checkWrite(filepath.Dir(link))); err != nil {
		return err
	}
	return os.Symlink(target, link)
}

// RemoveLink panics on a guard refusal and returns any other error.
// RemoveLink removes a symlink a test planted in its scratch directory. The guard
// checks the link's own directory: its target is outside on purpose.
func RemoveLink(path string) error {
	if err := mustGuard(checkWrite(filepath.Dir(path))); err != nil {
		return err
	}
	return os.Remove(path)
}

// RemoveAll panics on a guard refusal and returns any other error. It is os.RemoveAll behind the guard.
func RemoveAll(path string) error { return mustGuard(guardedRemoveAll(path)) }

// Grow panics on a guard refusal and returns any other error. It makes a file at least size bytes long, behind the guard.
func Grow(path string, size int64) error { return mustGuard(guardedGrow(path, size)) }

// ScratchTemp returns every failure, a refusal included, as an error.
// ScratchTemp is os.MkdirTemp under the system temp directory, registered
// as a scratch root.
func ScratchTemp(pattern string) (string, error) {
	dir, err := os.MkdirTemp("", pattern)
	if err != nil {
		return "", err
	}
	return registerScratch(dir)
}

// AssertNoLinks returns what it finds as an error wrapping ErrGuard, and never panics.
// AssertNoLinks walks a scratch root after the fixture is built: no symlink
// and no file sharing its inode anywhere in it, so nothing a gateway writes
// there can reach another file.
func AssertNoLinks(root string) error {
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

// UpdateGoldenEnv is the variable that lets a test rewrite its goldens: WriteGolden refuses
// without LOCALBASE_UPDATE_GOLDEN=1.
const UpdateGoldenEnv = "LOCALBASE_UPDATE_GOLDEN"

// WriteGolden returns a guard refusal as an error wrapping ErrGuard, and never panics.
// It writes a test's golden file under its package's testdata/golden (the test's working
// directory is its package), and only when LOCALBASE_UPDATE_GOLDEN=1: goldens are source files,
// so nothing else in a run may write them. A name that leaves testdata/golden is refused too.
func WriteGolden(name string, b []byte) error {
	if os.Getenv(UpdateGoldenEnv) != "1" {
		return fmt.Errorf("%w: %s is not 1, so goldens are read-only", ErrGuard, UpdateGoldenEnv)
	}
	dir, err := filepath.Abs(filepath.Join("testdata", "golden"))
	if err != nil {
		return err
	}
	// The name is checked before anything is made, so a refused one leaves no directory behind.
	if path := filepath.Join(dir, name); !within(path, dir) || path == dir {
		return fmt.Errorf("%w: golden %q leaves %s", ErrGuard, name, dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	root, err := registerScratch(dir)
	if err != nil {
		return err
	}
	return guardedWriteFile(filepath.Join(root, name), b, 0o644)
}

func realRootOf(root string) string {
	if real, err := filepath.EvalSymlinks(root); err == nil {
		return real
	}
	return root
}
