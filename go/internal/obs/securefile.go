package obs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"
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
	return checkOwnedRegular(path, info)
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

// syncFile is (*os.File).Sync, which on macOS is F_FULLFSYNC. A test can see it run, and fail it.
var syncFile = (*os.File).Sync

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
	// The data reaches the disk before the name does. Otherwise a crash can keep the rename and lose
	// the data, and a restore journal or a settings file is then there and empty.
	if err := syncFile(tmp); err != nil {
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
// stop the gateway from starting (CheckRootFiles); this stops just this one write.
func WritePrivateChecked(path string, data []byte) error {
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if err := checkOwnedRegular(path, info); err != nil {
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
		audit, _ := filepath.Glob(filepath.Join(logs, "audit*.jsonl"))
		for _, p := range append(matches, audit...) {
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
		checkFile(startRefusalPath(rootDir))
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

// startRefusal remembers why the gateway refused to start. A service manager retries
// a failing gateway every few seconds, so the record lets each cause be logged once,
// and lets doctor say how long it has been going on.
type startRefusal struct {
	Path    string `json:"path"`
	Problem string `json:"problem"`
	Since   string `json:"since"`
}

func startRefusalPath(rootDir string) string {
	return filepath.Join(rootDir, "runtime", "start-refusal.json")
}

// RecordStartRefusal notes a refusal and reports whether it is new: the first for
// this file and problem since the gateway last started. If it can't be recorded it
// counts as new, so the cause is still reported.
func RecordStartRefusal(rootDir string, cause *UnsafeFileError, now time.Time) bool {
	if prior := StartRefusal(rootDir); prior != nil && prior.Path == cause.Path && prior.Problem == cause.Problem {
		return false
	}
	b, err := json.Marshal(startRefusal{cause.Path, cause.Problem, now.UTC().Format(time.RFC3339)})
	if err != nil {
		return true
	}
	dir := filepath.Dir(startRefusalPath(rootDir))
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		_ = os.Mkdir(dir, 0o700) // a first start refused before anything made it; checked next
	}
	if info, err := os.Lstat(dir); err == nil && info.IsDir() {
		_ = WritePrivateAtomic(startRefusalPath(rootDir), b) // a note for doctor: the refusal itself stands
	}
	return true
}

// StartRefusal is the recorded refusal, or nil.
func StartRefusal(rootDir string) *startRefusal {
	b, err := ReadOwnedRegular(startRefusalPath(rootDir))
	if err != nil {
		return nil
	}
	var r startRefusal
	if json.Unmarshal(b, &r) != nil {
		return nil
	}
	return &r
}

// ClearStartRefusal is for a gateway that got past the check.
func ClearStartRefusal(rootDir string) { os.Remove(startRefusalPath(rootDir)) }

// LogStartRefusal writes the event if the log itself is safe to open. If the log is
// the unsafe file, the refusal on stderr is all there is.
func LogStartRefusal(rootDir string, logsDir string, cause *UnsafeFileError) {
	sink, err := OpenSink(logsDir)
	if err != nil {
		return
	}
	defer func() { _ = sink.Close() }()
	slog.New(NewHandler(sink, slog.LevelInfo)).Error("The gateway refused to start: "+cause.Error(),
		"event", "gateway.start-refused", "category", "runtime", "path", cause.Path, "problem", cause.Problem)
}
