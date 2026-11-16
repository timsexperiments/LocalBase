// Package busyread makes a database read busy, for tests: a read-only store reading in place beside
// an attached store, and another process holding the WAL index's locks. It is apart from testutil
// because it needs store, whose own tests import testutil.
//
// RecoveringElsewhere re-runs the test binary as its helper process, so a package that uses it
// needs this test, which does nothing unless it is that process:
//
//	func TestRecoveringHelper(t *testing.T) { busyread.RecoveringHelper(t) }
package busyread

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// recoveringEnv is set for the helper process RecoveringElsewhere starts.
const recoveringEnv = "LOCALBASE_TEST_RECOVERING"

// TestRecoveringHelper is that process: it acts like another connection rebuilding the WAL index of
// the database it's given. It holds the -shm's write lock (byte 120) and recovery lock (byte 122),
// and the first copy of the index header no longer matches the second, so a reader in another process
// can neither trust the index nor rebuild it: SQLITE_BUSY_RECOVERY, after the busy timeout. Under WAL a
// writer no longer makes a reader busy, so this is how a busy read happens now; a crashed root's first
// opener beside a second process does it for real. Found by Reliability. It writes a test root's -shm.
//
// RecoveringHelper is that test's body; each package's TestRecoveringHelper calls it.
func RecoveringHelper(t *testing.T) {
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

// RecoveringElsewhere starts that process on path and returns once it holds the locks. release ends
// it; the next read rebuilds the index from the -wal.
func RecoveringElsewhere(t *testing.T, path string) (release func()) {
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
	t.Fatal("the helper process ended without taking the locks (does this package have TestRecoveringHelper calling busyread.RecoveringHelper?)")
	return release
}

// AttachedRoot is a root with a writable store attached, so its -shm exists and a read-only store
// reads it in place, as beside a running gateway.
func AttachedRoot(t *testing.T) (dir string) {
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

// Read is a database read in place, beside an attached store; hold makes its next reads busy.
func Read(t *testing.T) (*store.Store, func()) {
	t.Helper()
	path := filepath.Join(AttachedRoot(t), "local-base.db")
	st, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, func() {
		t.Helper()
		RecoveringElsewhere(t, path)
	}
}
