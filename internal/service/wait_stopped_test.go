package service

import (
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/root"
)

// The stop is confirmed when the root has no owner and no backend port answers; else it says which
// is left once the time is up.
func TestWaitStopped(t *testing.T) {
	r, err := root.Init(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	never := func(int) bool { return false }
	if err := WaitStopped(r, []int{9000}, never, time.Second); err != nil {
		t.Errorf("stopped: %v", err)
	}
	always := func(int) bool { return true }
	if err := WaitStopped(r, []int{9000, 9002}, always, 300*time.Millisecond); err == nil || err.Error() != "something still listens on backend port 9000, 9002" {
		t.Errorf("a survivor: %v", err)
	}
	var calls atomic.Int32
	goneSoon := func(int) bool { return calls.Add(1) < 3 }
	if err := WaitStopped(r, []int{9000}, goneSoon, 5*time.Second); err != nil {
		t.Errorf("a survivor that goes: %v", err)
	}

	release, err := r.LockAs("127.0.0.1", 25411, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := WaitStopped(r, nil, never, 300*time.Millisecond); err == nil || err.Error() != "the gateway did not stop" {
		t.Errorf("still owned: %v", err)
	}
}
