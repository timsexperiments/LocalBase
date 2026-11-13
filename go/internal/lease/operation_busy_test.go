package lease

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// When something else holds the root, the error says so, for a caller that can wait.
func TestLeaseSaysWhenItIsBusy(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", dir)
	// A lease names a root by its path and doesn't touch the directory, so this needs no root.Init, which
	// this package can't import: root imports lease.
	rootDir := filepath.Join(dir, "root")
	old := operationWait
	operationWait = 100 * time.Millisecond
	t.Cleanup(func() { operationWait = old })
	held, err := AcquireOperation(rootDir, "first")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	_, err = AcquireOperation(rootDir, "second")
	if !errors.Is(err, ErrOperationBusy) {
		t.Fatalf("%v", err)
	}
	// A lease error of another kind is not that: only this type says so, and only for this.
	if errors.Is(&busyError{"x"}, errors.New("something else")) {
		t.Error("a busy error matches anything")
	}
}

// A lease waits 10 s for another operation before it says the root is busy.
func TestTheOperationWaitIsTenSeconds(t *testing.T) {
	if operationWait != 10*time.Second {
		t.Fatalf("a lease waits %v, want 10s", operationWait)
	}
}

// A busy root is refused at the wait a test sets, and not at the 10 s a lease waits for itself. The bounds are
// loose, for a loaded machine: at least the wait, and within ten times it and a quarter of the 10 s.
func TestABusyLeaseIsRefusedAtItsWait(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", dir)
	rootDir := filepath.Join(dir, "root")
	const wait = 100 * time.Millisecond
	old := operationWait
	operationWait = wait
	t.Cleanup(func() { operationWait = old })
	held, err := AcquireOperation(rootDir, "first")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	start := time.Now()
	_, err = AcquireOperation(rootDir, "second")
	took := time.Since(start)
	if !errors.Is(err, ErrOperationBusy) {
		t.Fatalf("%v", err)
	}
	if took < wait || took > 10*wait || took > 10*time.Second/4 {
		t.Errorf("refused after %v, want about %v", took, wait)
	}
}
