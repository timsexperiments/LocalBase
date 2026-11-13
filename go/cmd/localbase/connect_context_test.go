package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/placement/placementtest"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// With no gateway running, `connect` plans an imported chat model through the library, as the page
// does, and gives its real context. It used to plan with no catalog entry and say 0.
func TestConnectGivesAnImportedModelsContext(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	roomyHost(t)
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "root")
	if code, out := runApp(t, "--root", dir, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	weights := filepath.Join(tmp, "ctx-weights-q4_k_m.gguf")
	placementtest.WriteMoEGGUF(t, weights, 1<<20)
	if code, out := runApp(t, "--root", dir, "models", "import", weights, "--task", "chat", "--id", "ctx-import", "--name", "Ctx"); code != 0 {
		t.Fatalf("import: %s", out)
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
	e := &env{Root: r, Store: st, Catalog: cat, Config: cfg, Host: newHostProvider()}
	lib, err := openLibrary(e)
	if err != nil {
		t.Fatal(err)
	}
	llm, _, _ := placementLLM(e, lib)
	defer llm.Close()
	plan, err := llm.Plan("ctx-import")
	if err != nil || plan == nil || plan.Budget.EffectiveContext <= 0 {
		t.Fatalf("the library's plan: %+v, %v", plan, err)
	}
	if got := connectContext(e, "ctx-import"); got != plan.Budget.EffectiveContext {
		t.Errorf("connect's context %d, the plan's %d", got, plan.Budget.EffectiveContext)
	}
}
