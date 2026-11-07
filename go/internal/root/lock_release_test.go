package root

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func lockOwner(t *testing.T, r *Root) *Owner {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.Dir, "runtime", "gateway.lock", "owner.json"))
	if err != nil {
		return nil
	}
	var o Owner
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil
	}
	return &o
}

// A gateway's release takes away only its own lock. LockAs takes over a lock whose owner can't be
// read once it is 10 s old, even if that owner is still running. When that first gateway then
// stops, its release must leave the new owner's lock alone, as the TS gateway's release does.
func TestALockTakenOverSurvivesTheFirstOwnersRelease(t *testing.T) {
	r, err := Init(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	releaseFirst, err := r.LockAs("127.0.0.1", 28390, "", "")
	if err != nil {
		t.Fatal(err)
	}
	lockDir := filepath.Join(r.Dir, "runtime", "gateway.lock")
	// The first owner's record can't be read any more, and the lock is older than 10 s.
	if err := os.WriteFile(filepath.Join(lockDir, "owner.json"), []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(lockDir, old, old); err != nil {
		t.Fatal(err)
	}
	releaseSecond, err := r.LockAs("127.0.0.1", 28391, "", "")
	if err != nil {
		t.Fatalf("the second gateway didn't take over: %v", err)
	}
	second := lockOwner(t, r)
	if second == nil || second.Port != 28391 {
		t.Fatalf("after the takeover the owner is %+v", second)
	}

	releaseFirst()
	if now := lockOwner(t, r); now == nil || now.InstanceToken != second.InstanceToken {
		t.Fatalf("the first gateway's release took the second's lock: the owner is now %+v", now)
	}
	if state, _, _ := r.OwnerState(); state != "active" {
		t.Errorf("after the first release the lock is %q, want active", state)
	}

	releaseSecond()
	if _, err := os.Lstat(lockDir); !os.IsNotExist(err) {
		t.Errorf("the second gateway's release left its lock: %v", err)
	}
}

// A lock whose owner record can't be read isn't proven to be this gateway's, so its release leaves
// it, as the TS gateway's does. A later start reclaims it once it is 10 s old.
func TestAReleaseLeavesALockItCantProveIsItsOwn(t *testing.T) {
	r, err := Init(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	release, err := r.LockAs("127.0.0.1", 28393, "", "")
	if err != nil {
		t.Fatal(err)
	}
	lockDir := filepath.Join(r.Dir, "runtime", "gateway.lock")
	if err := os.WriteFile(filepath.Join(lockDir, "owner.json"), []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := os.Lstat(lockDir); err != nil {
		t.Errorf("the release removed a lock it couldn't read: %v", err)
	}
}

// A gateway that still owns its lock removes it on release, as before.
func TestAReleaseRemovesItsOwnLock(t *testing.T) {
	r, err := Init(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	release, err := r.LockAs("127.0.0.1", 28392, "", "")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if state, _, _ := r.OwnerState(); state != "missing" {
		t.Errorf("after release the lock is %q, want missing", state)
	}
}
