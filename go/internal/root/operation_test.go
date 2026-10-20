package root

import (
	"os"
	"os/exec"
	"path/filepath"
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
	go func() {
		l2, err := AcquireOperation(rootDir, "second")
		if err != nil {
			t.Error(err)
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
	time.Sleep(50 * time.Millisecond)
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
