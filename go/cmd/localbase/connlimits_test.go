package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

func TestSavedConnLimit(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if got := savedConnLimit(st, store.MaxConnectionsKey, 4096); got != 4096 {
		t.Errorf("nothing saved: %d", got)
	}
	for value, want := range map[string]int{"100": 100, "0": 0, "-1": 4096, "many": 4096} {
		if err := st.SetSetting(context.Background(), store.MaxConnectionsKey, value); err != nil {
			t.Fatal(err)
		}
		if got := savedConnLimit(st, store.MaxConnectionsKey, 4096); got != want {
			t.Errorf("saved %q: %d, want %d", value, got, want)
		}
	}
}
