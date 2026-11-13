// Package safefile opens, reads and writes this user's own plain files without letting anything planted
// at a path (a link, a FIFO, another user's file, a second hard link) redirect a write or a truncate.
package safefile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
)

// UnsafeFileError is a log or checkpoint path that cannot be trusted: something
// other than this user's own plain file, which could send a write or a truncate
// somewhere else. The gateway refuses to start on one (DESIGN §27), as TS does.
type UnsafeFileError struct {
	Path    string `json:"path"`
	Problem string `json:"problem"`
}

func (e *UnsafeFileError) Error() string {
	if e.Problem == hardLinkProblem {
		return fmt.Sprintf("Refusing to use %s: %s. That means a second name points at the same file, which a backup made with `ln` does. Delete the other name, or replace it with a real copy (`cp`), then try again.", e.Path, e.Problem)
	}
	return fmt.Sprintf("Refusing to use %s: %s. Remove it, check who can write to %s, then try again.", e.Path, e.Problem, filepath.Dir(e.Path))
}

const hardLinkProblem = "it has another hard link"

// options are the points in this package a test reaches into. Its zero value is how the package
// runs, and the exported functions pass that, so nothing here can be set from outside.
type options struct {
	uid        func() int           // (tests) whose files count as ours; nil is os.Getuid
	beforeOpen func()               // (tests) runs between the checks and the open: a swap the checks can't see
	afterOpen  func()               // (tests) runs between the open and the re-checks: a swap after the open
	sync       func(*os.File) error // (tests) nil is (*os.File).Sync, which on macOS is F_FULLFSYNC
}

func (o options) currentUID() int {
	if o.uid != nil {
		return o.uid()
	}
	return os.Getuid()
}

func (o options) fsync(f *os.File) error {
	if o.sync != nil {
		return o.sync(f)
	}
	return f.Sync()
}

// checkOwnedRegular is TS's ownedRegularFile.
func checkOwnedRegular(path string, info fs.FileInfo, o options) error {
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return &UnsafeFileError{path, "it is a symbolic link"}
	case !info.Mode().IsRegular():
		return &UnsafeFileError{path, "it is not a regular file"}
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return &UnsafeFileError{path, "its owner cannot be read"}
	}
	if int(st.Uid) != o.currentUID() {
		return &UnsafeFileError{path, "another user owns it"}
	}
	if st.Nlink > 1 {
		return &UnsafeFileError{path, hardLinkProblem}
	}
	return nil
}

// CheckOwnedRegular reports an unsafe file at path; a missing one is fine.
func CheckOwnedRegular(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return checkOwnedRegular(path, info, options{})
}

// OpenOwnedRegular opens this user's own plain file without following a link, and
// checks the path still names what it opened (TS openOwnedRegularFile). The file ends
// up 0600. A missing file is fs.ErrNotExist unless flags create it. Keep using the
// returned handle rather than the path.
func OpenOwnedRegular(path string, flags int) (*os.File, error) {
	return openOwnedRegular(path, flags, options{})
}

// openOwnedRegular is OpenOwnedRegular, with the points a test reaches into.
func openOwnedRegular(path string, flags int, o options) (*os.File, error) {
	before, err := os.Lstat(path)
	switch {
	case err == nil:
		if err := checkOwnedRegular(path, before, o); err != nil {
			return nil, err
		}
	case !errors.Is(err, fs.ErrNotExist) || flags&os.O_CREATE == 0:
		return nil, err
	default:
		before = nil
	}
	if o.beforeOpen != nil {
		o.beforeOpen()
	}
	// O_NOFOLLOW covers a link planted after the check above, which could otherwise
	// truncate its target before any later check runs. O_NONBLOCK: a FIFO swapped in
	// after the check must not hang the open.
	f, err := os.OpenFile(path, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EMLINK) {
			return nil, &UnsafeFileError{path, "it is a symbolic link"}
		}
		return nil, err
	}
	if o.afterOpen != nil {
		o.afterOpen()
	}
	fail := func(err error) (*os.File, error) {
		f.Close()
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	if err := checkOwnedRegular(path, opened, o); err != nil {
		return fail(err)
	}
	after, err := os.Lstat(path)
	if err != nil {
		return fail(err)
	}
	if err := checkOwnedRegular(path, after, o); err != nil {
		return fail(err)
	}
	if !os.SameFile(opened, after) || (before != nil && !os.SameFile(before, after)) {
		return fail(&UnsafeFileError{path, "it changed while being opened"})
	}
	if err := f.Chmod(0o600); err != nil {
		return fail(err)
	}
	return f, nil
}

// ReadOwnedRegular is os.ReadFile through OpenOwnedRegular.
func ReadOwnedRegular(path string) ([]byte, error) {
	f, err := OpenOwnedRegular(path, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// WritePrivateAtomic replaces path with a new private file. The temporary name is
// fresh and exclusive, so nothing planted can redirect the write, and the rename
// replaces a link at path rather than following it.
func WritePrivateAtomic(path string, data []byte) error {
	return writePrivateAtomic(path, data, options{})
}

// writePrivateAtomic is WritePrivateAtomic, with the sync a test can see run, and fail.
func writePrivateAtomic(path string, data []byte, o options) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// The data reaches the disk before the name does. Otherwise a crash can keep the rename and lose
	// the data, and a restore journal or a settings file is then there and empty.
	if err := o.fsync(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// WritePrivateChecked is WritePrivateAtomic for settings and caches: a planted link or
// other unsafe file at path fails the write with a clear error, instead of being quietly
// replaced, and whatever it points at stays untouched. Only logs, stderr and checkpoints
// stop the gateway from starting (obs.CheckRootFiles); this stops just this one write.
func WritePrivateChecked(path string, data []byte) error {
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if err := checkOwnedRegular(path, info, options{}); err != nil {
			return err
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	return WritePrivateAtomic(path, data)
}

// WarnRefusedWrite logs a refused write for a caller that otherwise ignores write
// failures, so a planted link there can still be found. Path and reason only. Other
// errors stay as quiet as they were.
func WarnRefusedWrite(log *slog.Logger, err error) {
	var unsafe *UnsafeFileError
	if !errors.As(err, &unsafe) {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	log.Warn("A write was refused: "+unsafe.Error(), "event", "root.write-refused", "path", unsafe.Path, "problem", unsafe.Problem)
}
