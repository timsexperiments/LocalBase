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
