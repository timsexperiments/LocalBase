package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// The placement commands plan against this computer less what the configuration keeps for it, the
// same reserve the gateway admits with.
func TestPlacementLLMKeepsTheConfiguredReserve(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	roomyHost(t)
	dir := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", dir, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	r, err := root.Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cat, err := catalog.LoadRoot(r.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := st.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cfg.SystemReserve.Percent, cfg.SystemReserve.MinimumGB = 30, 3
	e := &env{Root: r, Store: st, Catalog: cat, Config: cfg, Host: testHost(t)}
	lib, err := openLibrary(e)
	if err != nil {
		t.Fatal(err)
	}
	llm, mem, _ := placementLLM(e, lib)
	defer llm.Close()
	want := memory.NewController(e.Host, memory.Reserve{Percent: 30, MinimumGB: 3}, false).ReserveBytes()
	if got := mem.ReserveBytes(); got != want || got == memory.NewController(e.Host, memory.Reserve{}, false).ReserveBytes() {
		t.Errorf("reserve %d, want %d", got, want)
	}
}
