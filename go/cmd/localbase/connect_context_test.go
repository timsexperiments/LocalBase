package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
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
	if got, planned := connectContext(e, "ctx-import"); got != plan.Budget.EffectiveContext || !planned {
		t.Errorf("connect's context %d (planned %v), the plan's %d", got, planned, plan.Budget.EffectiveContext)
	}
	// And connect says the figure is a plan, not the running value: in its overview, beside a client's
	// snippets, and in its JSON.
	want := fmt.Sprintf("%d tokens, as planned without a running gateway", plan.Budget.EffectiveContext)
	if code, out := runApp(t, "--root", dir, "connect", "--model", "ctx-import"); code != 0 || !strings.Contains(out, "Context: "+want+"\n") {
		t.Errorf("overview: %d\n%s", code, out)
	}
	if code, out := runApp(t, "--root", dir, "connect", "opencode", "--model", "ctx-import"); code != 0 || !strings.Contains(out, "Context in these snippets: "+want+".") {
		t.Errorf("a client: %d\n%s", code, out)
	}
	// Only beside snippets that hold the figure: curl's don't.
	if code, out := runApp(t, "--root", dir, "connect", "curl", "--model", "ctx-import"); code != 0 || strings.Contains(out, "Context in these snippets") {
		t.Errorf("a client without the figure: %d\n%s", code, out)
	}
	code, out := runApp(t, "--root", dir, "--json", "connect", "--model", "ctx-import")
	var env struct {
		Data struct {
			Context struct {
				Tokens int    `json:"tokens"`
				Source string `json:"source"`
			} `json:"context"`
		} `json:"data"`
	}
	if code != 0 || json.NewDecoder(strings.NewReader(out[strings.Index(out, "{"):])).Decode(&env) != nil ||
		env.Data.Context.Tokens != plan.Budget.EffectiveContext || env.Data.Context.Source != "planned" {
		t.Errorf("--json: %d %+v\n%s", code, env.Data.Context, out)
	}
}
