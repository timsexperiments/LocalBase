package obs

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/safefile"
)

// CheckRootFiles finds what would stop the gateway: an unsafe log, stderr capture or
// checkpoint path, or one of their directories that is not this user's real
// directory. It changes nothing, so doctor reports it without starting anything.
func CheckRootFiles(rootDir string) []*safefile.UnsafeFileError {
	var out []*safefile.UnsafeFileError
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
		case !ok || int(st.Uid) != os.Getuid():
			problem = "another user owns it"
		}
		if problem != "" {
			out = append(out, &safefile.UnsafeFileError{Path: dir, Problem: problem})
			return false
		}
		return true
	}
	checkFile := func(path string) {
		var unsafe *safefile.UnsafeFileError
		if errors.As(safefile.CheckOwnedRegular(path), &unsafe) {
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
func RecordStartRefusal(rootDir string, cause *safefile.UnsafeFileError, now time.Time) bool {
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
		_ = safefile.WritePrivateAtomic(startRefusalPath(rootDir), b) // a note for doctor: the refusal itself stands
	}
	return true
}

// StartRefusal is the recorded refusal, or nil.
func StartRefusal(rootDir string) *startRefusal {
	b, err := safefile.ReadOwnedRegular(startRefusalPath(rootDir))
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
func LogStartRefusal(rootDir string, logsDir string, cause *safefile.UnsafeFileError) {
	sink, err := OpenSink(logsDir)
	if err != nil {
		return
	}
	defer func() { _ = sink.Close() }()
	slog.New(NewHandler(sink, slog.LevelInfo)).Error("The gateway refused to start: "+cause.Error(),
		"event", "gateway.start-refused", "category", "runtime", "path", cause.Path, "problem", cause.Problem)
}
