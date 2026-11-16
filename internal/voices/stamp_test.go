package voices

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// The stamp names each stored voice by id and content, oldest first, so it changes when the set
// does; the warm speech server restarts on that.
func TestStamp(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "localbase.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if got := Stamp(ctx, st); got != "" {
		t.Errorf("no voices: %q", got)
	}
	v, err := New(st, dir).Create(ctx, "First", true, clip(1), "test")
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := st.Voices(ctx)
	one := v.ID + ":" + stored[0].SHA256 + ";"
	if got := Stamp(ctx, st); got != one {
		t.Errorf("one voice: %q, want %q", got, one)
	}
	if _, err := New(st, dir).Create(ctx, "Second", true, clip(2), "test"); err != nil {
		t.Fatal(err)
	}
	if got := Stamp(ctx, st); got == one || len(got) <= len(one) || got[:len(one)] != one {
		t.Errorf("two voices: %q", got)
	}
}
