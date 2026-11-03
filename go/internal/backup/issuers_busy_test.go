package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A root's bindings decide who can sign in, so a busy moment must not read as having none: every
// binding in the backup would then be reported as coming back, or as nothing to report.
func TestBusyBindingsAreNeverReadAsNone(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	ctx := context.Background()
	stagedCopy := filepath.Join(dir(t), "staged.db")
	body, err := os.ReadFile(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stagedCopy, body, 0o600); err != nil {
		t.Fatal(err)
	}
	quiet, err := openReadOnly(stagedCopy)
	if err != nil {
		t.Fatal(err)
	}
	defer quiet.Close()
	holdLock(t, r.DB())
	shortBusyWaits(t, 0)
	target, err := openReadOnly(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if _, err := bindingKeys(ctx, target); !isBusy(err) {
		t.Errorf("bindingKeys: %v", err)
	}
	if _, err := returningBindings(ctx, quiet, target); !isBusy(err) {
		t.Errorf("returningBindings: %v", err)
	}
}
