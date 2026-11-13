package diagnostics

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// The busy-read helpers below are copies of cmd/localbase/busy_test.go's, which its other busy tests
// still use: a test can't import another package's _test files.

// recoveringEnv is set for the helper process recoveringElsewhere starts.
const recoveringEnv = "LOCALBASE_TEST_RECOVERING"

// TestRecoveringHelper is that process: it acts like another connection rebuilding the WAL index of
// the database it's given. It holds the -shm's write lock (byte 120) and recovery lock (byte 122),
// and the first copy of the index header no longer matches the second, so a reader in another process
// can neither trust the index nor rebuild it: SQLITE_BUSY_RECOVERY, after the busy timeout. Under WAL a
// writer no longer makes a reader busy, so this is how a busy read happens now; a crashed root's first
// opener beside a second process does it for real. Found by Reliability. It writes a test root's -shm.
func TestRecoveringHelper(t *testing.T) {
	path := os.Getenv(recoveringEnv)
	if path == "" {
		t.Skip("only as the helper process")
	}
	f, err := os.OpenFile(path+"-shm", os.O_RDWR, 0)
	if err != nil {
		fmt.Println("FAILED", err)
		return
	}
	for _, b := range []int64{120, 122} {
		lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: io.SeekStart, Start: b, Len: 1}
		if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lock); err != nil {
			fmt.Println("FAILED", err)
			return
		}
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xFF}, 48), 0); err != nil {
		fmt.Println("FAILED", err)
		return
	}
	fmt.Println("RECOVERING")
	_, _ = io.ReadAll(os.Stdin) // until released
	_ = f.Close()
}

// recoveringElsewhere starts that process on path and returns once it holds the locks. release ends
// it; the next read rebuilds the index from the -wal.
func recoveringElsewhere(t *testing.T, path string) (release func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRecoveringHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), recoveringEnv+"="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	released := false
	release = func() {
		if !released {
			released = true
			_ = stdin.Close()
			_ = cmd.Wait()
		}
	}
	t.Cleanup(release)
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "RECOVERING" {
			return release
		}
		if strings.HasPrefix(line, "FAILED") {
			t.Fatal(line)
		}
	}
	t.Fatal("the helper process ended without taking the locks")
	return release
}

// attachedRoot is a root with a writable store attached, so its -shm exists and a read-only store
// reads it in place, as beside a running gateway.
func attachedRoot(t *testing.T) (dir string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "root")
	r, err := root.Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	attached, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { attached.Close() })
	return dir
}

// busyRead is a database read in place, beside an attached store; hold makes its next reads busy.
func busyRead(t *testing.T) (*store.Store, func()) {
	t.Helper()
	path := filepath.Join(attachedRoot(t), "local-base.db")
	st, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, func() {
		t.Helper()
		recoveringElsewhere(t, path)
	}
}

// And a peak read that fails partway, which used to be silent.
func TestDiagnosticModelsListsAFailedPeakRead(t *testing.T) {
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	var spec *catalog.Spec
	for _, s := range cat.All() {
		if s.Primary().SHA256 != "" {
			spec = s
			break
		}
	}
	st, hold := busyRead(t)
	hold()
	problems := &readProblems{dir: t.TempDir()}
	cfg := &store.Config{Active: map[string]string{spec.Kind: spec.ModelID}, Selected: map[string][]string{spec.Kind: {spec.ModelID}}}
	diagnosticModels(context.Background(), problems.dir, st, cat, cfg, 64, problems)
	if len(problems.text) == 0 || !strings.Contains(strings.Join(problems.text, "\n"), "reading the memory peaks seen for "+spec.ModelID+": the database is busy") {
		t.Errorf("%v", problems.text)
	}
}
