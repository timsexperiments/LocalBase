package root

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestOperationLease(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	rootDir := filepath.Join(t.TempDir(), "root")
	l, err := AcquireOperation(rootDir, "configure")
	if err != nil {
		t.Fatal(err)
	}
	p := operationPaths(rootDir)
	var o operationOwner
	if !readJSON(p.owner, &o) || !o.valid() || o.Operation != "configure" || o.RootHash != Hash(rootDir) {
		t.Fatalf("owner %+v", o)
	}
	// A second operation waits for the first.
	got := make(chan time.Time, 1)
	released2 := make(chan struct{})
	go func() {
		defer close(released2)
		l2, err := AcquireOperation(rootDir, "second")
		if err != nil {
			t.Error(err)
			return
		}
		got <- time.Now()
		l2.Release()
	}()
	time.Sleep(200 * time.Millisecond)
	released := time.Now()
	l.Release()
	if at := <-got; at.Before(released) {
		t.Error("second operation did not wait")
	}
	// The second lease is checked once its Release has returned, not after a fixed pause: a release
	// syncs what it writes, and on a slow runner that can outlast any pause chosen here.
	select {
	case <-released2:
	case <-time.After(30 * time.Second):
		t.Fatal("the second operation's release never returned")
	}
	if isDir(p.lock) {
		t.Error("lease left behind")
	}

	// A lease whose process is gone is reclaimed.
	cmd := exec.Command("true")
	cmd.Run()
	os.Mkdir(p.lock, 0o700)
	writeJSONAtomic(p.owner, operationOwner{Version: 1, OperationID: uuid(), Operation: "dead", Root: rootDir,
		RootHash: Hash(rootDir), PID: cmd.Process.Pid, StartedAt: nowISO()})
	l, err = AcquireOperation(rootDir, "after-crash")
	if err != nil {
		t.Fatal(err)
	}

	// Service start hands the lease to the gateway holding the token.
	if err := l.Handoff("11111111-2222-4333-8444-555555555555"); err != nil {
		t.Fatal(err)
	}
	l.Release() // no-op once handed off
	if !isDir(p.lock) {
		t.Fatal("handed-off lease released")
	}
	served, err := ServeInitialization(rootDir, "11111111-2222-4333-8444-555555555555")
	if err != nil || !served.claimed {
		t.Fatalf("claim: %v %+v", err, served)
	}
	readJSON(p.owner, &o)
	if o.Operation != "serve-initialize" || o.HandoffToken != "" || o.PID != os.Getpid() {
		t.Errorf("claimed owner %+v", o)
	}
	served.Release()
	if isDir(p.lock) {
		t.Error("claimed lease left behind")
	}
	// Without a handoff to claim, a service gateway takes a fresh lease.
	served, err = ServeInitialization(rootDir, "11111111-2222-4333-8444-555555555555")
	if err != nil || served.claimed {
		t.Fatalf("fresh: %v", err)
	}
	served.Release()
}

// A lease is released even when the mutation lock cannot be taken. Taking it
// needs disk space, and on a full disk the failure used to be ignored: the lease
// stayed with this process, alive, so nothing could ever reclaim it, and every
// operation on the root timed out until the gateway restarted.
func TestReleaseWithoutMutationLock(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	rootDir := filepath.Join(t.TempDir(), "root")
	l, err := AcquireOperation(rootDir, "install model")
	if err != nil {
		t.Fatal(err)
	}
	// Someone alive holds the mutation lock, so it cannot be taken, as when
	// creating its directory fails.
	p := operationPaths(rootDir)
	if err := os.Mkdir(p.mutation, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(filepath.Join(p.mutation, "owner.json"), mutationOwner{1, "held", os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	l.paths.mutationWait = 50 * time.Millisecond
	l.Release()
	if isDir(p.lock) {
		t.Fatal("the lease is still held")
	}
	os.RemoveAll(p.mutation)
	began := time.Now()
	l2, err := AcquireOperation(rootDir, "next")
	if err != nil {
		t.Fatalf("the next operation: %v", err)
	}
	l2.Release()
	if took := time.Since(began); took > time.Second {
		t.Errorf("the next operation waited %v", took)
	}
}

// Releasing without the mutation lock only ever removes this lease: another
// operation's lease stays, even from a stale handle.
func TestReleaseWithoutMutationLockKeepsAnotherLease(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	rootDir := filepath.Join(t.TempDir(), "root")
	stale, err := AcquireOperation(rootDir, "first")
	if err != nil {
		t.Fatal(err)
	}
	stale.Release()
	current, err := AcquireOperation(rootDir, "second")
	if err != nil {
		t.Fatal(err)
	}
	defer current.Release()
	p := operationPaths(rootDir)
	if err := os.Mkdir(p.mutation, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(filepath.Join(p.mutation, "owner.json"), mutationOwner{1, "held", os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(p.mutation)
	stale.paths.mutationWait = 50 * time.Millisecond
	stale.Release()
	if !isDir(p.lock) {
		t.Error("a stale release removed another operation's lease")
	}
}

// A removal that fails, as it can on a full copy-on-write volume, keeps trying:
// the lease clears by itself once the removal can happen, with no restart.
func TestReleaseRetriesARemovalThatFails(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	rootDir := filepath.Join(t.TempDir(), "root")
	l, err := AcquireOperation(rootDir, "install model")
	if err != nil {
		t.Fatal(err)
	}
	var fails atomic.Int32
	fails.Store(3)
	l.remove = func(p string) error {
		if fails.Add(-1) >= 0 {
			// As a partial removal does: the owner record goes first.
			os.Remove(filepath.Join(p, "owner.json"))
			return syscall.ENOSPC
		}
		return os.RemoveAll(p)
	}
	l.retry = 10 * time.Millisecond
	p := operationPaths(rootDir)
	l.Release()
	deadline := time.Now().Add(5 * time.Second)
	for isDir(p.lock) {
		if time.Now().After(deadline) {
			t.Fatal("the lease never cleared")
		}
		time.Sleep(5 * time.Millisecond)
	}
	l2, err := AcquireOperation(rootDir, "next")
	if err != nil {
		t.Fatalf("the next operation: %v", err)
	}
	l2.Release()
}
