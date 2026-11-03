package store

import (
	"context"
	"path/filepath"
	"testing"
)

// The opt-in is off unless it says on: no value, an old value or a typo keeps it off.
func TestPrivateImportsOptIn(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if st.PrivateImportsAllowed(ctx) {
		t.Error("on with nothing saved")
	}
	for value, want := range map[string]bool{"on": true, "off": false, "": false, "true": false, "ON": false} {
		if err := st.SetSetting(ctx, PrivateImportsKey, value); err != nil {
			t.Fatal(err)
		}
		if got := st.PrivateImportsAllowed(ctx); got != want {
			t.Errorf("saved %q: %v, want %v", value, got, want)
		}
	}
}

// The two opt-ins are separate: allowing a LAN mirror does not allow reading files.
func TestImportOptInsAreSeparate(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.SetSetting(ctx, PrivateImportsKey, "on"); err != nil {
		t.Fatal(err)
	}
	if st.LocalPathImportsAllowed(ctx) {
		t.Error("the private-address opt-in allowed local paths")
	}
	if err := st.SetSetting(ctx, PrivateImportsKey, "off"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(ctx, LocalPathImportsKey, "on"); err != nil {
		t.Fatal(err)
	}
	if st.PrivateImportsAllowed(ctx) || !st.LocalPathImportsAllowed(ctx) {
		t.Errorf("private %v, local paths %v", st.PrivateImportsAllowed(ctx), st.LocalPathImportsAllowed(ctx))
	}
}
