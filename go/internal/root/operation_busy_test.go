package root

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
	r, err := Init(filepath.Join(dir, "root"))
	if err != nil {
		t.Fatal(err)
	}
	old := operationWait
	operationWait = 100 * time.Millisecond
	t.Cleanup(func() { operationWait = old })
	held, err := AcquireOperation(r.Dir, "first")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	_, err = AcquireOperation(r.Dir, "second")
	if !errors.Is(err, ErrOperationBusy) {
		t.Fatalf("%v", err)
	}
	// A lease error of another kind is not that: only this type says so, and only for this.
	if errors.Is(&busyError{"x"}, errors.New("something else")) {
		t.Error("a busy error matches anything")
	}
}
