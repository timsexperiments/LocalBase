package obs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	return fmt.Sprintf("Refusing to use %s: %s. Remove it, check who can write to %s, then try again.", e.Path, e.Problem, filepath.Dir(e.Path))
}

var (
	currentUID = os.Getuid
	// These run around the open (tests: a swap in the window the checks cannot see).
	beforeSecureOpen = func() {}
	afterSecureOpen  = func() {}
)

// checkOwnedRegular is TS's ownedRegularFile.
func checkOwnedRegular(path string, info fs.FileInfo) error {
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
	if int(st.Uid) != currentUID() {
		return &UnsafeFileError{path, "another user owns it"}
	}
	if st.Nlink > 1 {
		return &UnsafeFileError{path, "it has another hard link"}
	}
	return nil
}

// OpenOwnedRegular opens this user's own plain file without following a link, and
// checks the path still names what it opened (TS openOwnedRegularFile). The file ends
// up 0600. A missing file is fs.ErrNotExist unless flags create it. Keep using the
// returned handle rather than the path.
func OpenOwnedRegular(path string, flags int) (*os.File, error) {
	before, err := os.Lstat(path)
	switch {
	case err == nil:
		if err := checkOwnedRegular(path, before); err != nil {
			return nil, err
		}
	case !errors.Is(err, fs.ErrNotExist) || flags&os.O_CREATE == 0:
		return nil, err
	default:
		before = nil
	}
	beforeSecureOpen()
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
	afterSecureOpen()
	fail := func(err error) (*os.File, error) {
		f.Close()
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	if err := checkOwnedRegular(path, opened); err != nil {
		return fail(err)
	}
	after, err := os.Lstat(path)
	if err != nil {
		return fail(err)
	}
	if err := checkOwnedRegular(path, after); err != nil {
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
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// CheckRootFiles finds what would stop the gateway: an unsafe log, stderr capture or
// checkpoint path, or one of their directories that is not this user's real
// directory. It changes nothing, so doctor reports it without starting anything.
func CheckRootFiles(rootDir string) []*UnsafeFileError {
	var out []*UnsafeFileError
	checkDir := func(dir string) bool {
		info, err := os.Lstat(dir)
		if err != nil {
			return false
		}
		var problem string
		st, ok := info.Sys().(*syscall.Stat_t)
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			problem = "it is a symbolic link"
		case !info.IsDir():
			problem = "it is not a directory"
		case !ok || int(st.Uid) != currentUID():
			problem = "another user owns it"
		}
		if problem != "" {
			out = append(out, &UnsafeFileError{dir, problem})
			return false
		}
		return true
	}
	checkFile := func(path string) {
		info, err := os.Lstat(path)
		if err != nil {
			return
		}
		var unsafe *UnsafeFileError
		if errors.As(checkOwnedRegular(path, info), &unsafe) {
			out = append(out, unsafe)
		}
	}
	logs := filepath.Join(rootDir, "logs")
	if checkDir(logs) {
		matches, _ := filepath.Glob(filepath.Join(logs, "events*.jsonl"))
		for _, p := range matches {
			checkFile(p)
		}
		checkFile(filepath.Join(logs, "events.jsonl"))
		active, archive := stderrPaths(rootDir)
		checkFile(active)
		checkFile(archive)
	}
	runs := filepath.Join(rootDir, "runs")
	if checkDir(runs) {
		matches, _ := filepath.Glob(filepath.Join(runs, "*.json"))
		for _, p := range matches {
			checkFile(p)
		}
	}
	runtimeDir := filepath.Join(rootDir, "runtime")
	if checkDir(runtimeDir) {
		checkFile(filepath.Join(runtimeDir, "model-operations.json"))
	}
	// A glob and the explicit path can name one file twice.
	seen := map[string]bool{}
	unique := out[:0]
	for _, e := range out {
		if !seen[e.Path] {
			seen[e.Path] = true
			unique = append(unique, e)
		}
	}
	return unique
}
