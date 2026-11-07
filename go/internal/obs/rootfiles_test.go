package obs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/safefile"
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
	var unsafe *safefile.UnsafeFileError
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
		if m := fileMode(t, filepath.Join(dir, name)); m != 0o600 {
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

func TestCheckRootFiles(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"logs", "runs", "runtime"} {
		os.Mkdir(filepath.Join(root, d), 0o700)
	}
	for _, f := range []string{"logs/events.jsonl", "logs/events.1.jsonl", "logs/audit.jsonl", "logs/audit.1.jsonl", "logs/service-stderr.log", "runs/run_a.json", "runtime/model-operations.json"} {
		os.WriteFile(filepath.Join(root, f), []byte("x"), 0o600)
	}
	if got := CheckRootFiles(root); len(got) != 0 {
		t.Errorf("a normal root: %v", got)
	}
	if got := CheckRootFiles(t.TempDir()); len(got) != 0 {
		t.Errorf("an empty root: %v", got)
	}
	target, content := victim(t)
	for _, rel := range []string{"logs/events.jsonl", "logs/events.2.jsonl", "logs/audit.jsonl", "logs/audit.2.jsonl", "logs/service-stderr.log", "logs/service-stderr.log.1", "runs/run_a.json", "runtime/model-operations.json"} {
		p := filepath.Join(root, rel)
		os.Remove(p)
		os.Symlink(target, p)
	}
	got := CheckRootFiles(root)
	if len(got) != 8 {
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

// Retries of the same cause log once; a new cause, or a clean start, resets it.
func TestStartRefusalLoggedOncePerCause(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "runtime"), 0o700)
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	a := &safefile.UnsafeFileError{Path: filepath.Join(root, "runs", "run_a.json"), Problem: "it is a symbolic link"}
	if !RecordStartRefusal(root, a, now) {
		t.Error("the first refusal should be new")
	}
	for range 5 {
		if RecordStartRefusal(root, a, now.Add(time.Minute)) {
			t.Error("a retry of the same cause was new")
		}
	}
	if got := StartRefusal(root); got == nil || got.Since != "2026-11-02T12:00:00Z" {
		t.Errorf("since should stay the first time: %+v", got)
	}
	if !RecordStartRefusal(root, &safefile.UnsafeFileError{Path: a.Path, Problem: "it has another hard link"}, now) {
		t.Error("another problem should be new")
	}
	ClearStartRefusal(root)
	if StartRefusal(root) != nil {
		t.Error("not cleared")
	}
	if !RecordStartRefusal(root, a, now) {
		t.Error("after a clean start it should be new again")
	}
	// With no runtime directory yet, it makes one.
	fresh := t.TempDir()
	RecordStartRefusal(fresh, a, now)
	if StartRefusal(fresh) == nil {
		t.Error("a first start's refusal was not recorded")
	}
	// A linked runtime directory can't be recorded through, and still counts as new.
	linked := t.TempDir()
	elsewhere := t.TempDir()
	os.Symlink(elsewhere, filepath.Join(linked, "runtime"))
	for range 2 {
		if !RecordStartRefusal(linked, a, now) {
			t.Error("an unrecorded refusal should count as new")
		}
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Errorf("wrote through a linked directory: %v", entries)
	}
}

func TestLogStartRefusal(t *testing.T) {
	root := t.TempDir()
	logs := filepath.Join(root, "logs")
	cause := &safefile.UnsafeFileError{Path: filepath.Join(root, "runs", "run_a.json"), Problem: "it is a symbolic link"}
	LogStartRefusal(root, logs, cause)
	got, _ := os.ReadFile(filepath.Join(logs, "events.jsonl"))
	if !strings.Contains(string(got), "gateway.start-refused") || !strings.Contains(string(got), "run_a.json") {
		t.Errorf("%s", got)
	}
	// When the log is the unsafe file, nothing is written through it.
	target, content := victim(t)
	root = t.TempDir()
	logs = filepath.Join(root, "logs")
	os.Mkdir(logs, 0o700)
	os.Symlink(target, filepath.Join(logs, "events.jsonl"))
	LogStartRefusal(root, logs, &safefile.UnsafeFileError{Path: filepath.Join(logs, "events.jsonl"), Problem: "it is a symbolic link"})
	victimUntouched(t, target, content)
}
