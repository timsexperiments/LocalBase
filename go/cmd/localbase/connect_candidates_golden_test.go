package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/acquire"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// `connect`'s installed chat models and its default, recorded before their candidate rule moved to
// library (5-7f), on the same root as web's TestChatCandidatesGolden: two user catalog LLMs (tiny-chat
// with its file, zero-chat with its one file at 0 bytes and no expected size), a built-in one not
// installed, and three imports (one with its file, one without, one with tiny-chat's id). No gateway
// runs, so the default is the active model. LOCALBASE_UPDATE_CANDIDATES_GOLDEN=1 rewrites
// testdata/connect_candidates.golden.
func TestConnectCandidatesGolden(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	roomyHost(t)
	dir := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", dir, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "catalog", "data", "llm", "qwen3.5-4b-q4_k_m.json"))
	if err != nil {
		t.Fatal(err)
	}
	for id, size := range map[string]int{"tiny-chat": 4, "zero-chat": 0} {
		v, err := jsjson.Parse(b)
		if err != nil {
			t.Fatal(err)
		}
		o := v.(*jsjson.Object)
		o.Set("modelId", id)
		o.Set("displayName", id)
		o.Set("artifacts", []any{jsjson.FromGo(map[string]any{"sourcePath": id + ".gguf", "filename": id + ".gguf", "expectedSizeBytes": size,
			"sha256": strings.Repeat("a", 64), "role": "primary"})})
		if _, _, err := catalog.WriteUserEntry(dir, o); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	llmDir := filepath.Join(dir, "models", "llm")
	os.MkdirAll(llmDir, 0o700)
	os.WriteFile(filepath.Join(llmDir, "tiny-chat.gguf"), []byte("GGUF"), 0o600)
	os.WriteFile(filepath.Join(llmDir, "zero-chat.gguf"), nil, 0o600)
	st, err := store.Open(filepath.Join(dir, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := st.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Selected["llm"] = []string{"zero-chat", "qwen3.5-4b-q4_k_m", "tiny-chat"}
	cfg.Active["llm"] = "tiny-chat"
	if err := st.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	save := func(m models.Model) {
		b, _ := json.Marshal(m)
		if err := st.SaveImportedModel(context.Background(), m.ID, b); err != nil {
			t.Fatal(err)
		}
	}
	variant := []models.Variant{{ID: "q4", Artifacts: []models.Artifact{{Filename: "w.gguf", Role: "weights", Required: true}}}}
	save(models.Model{ID: "imp-ready", Tasks: []string{"chat"}, Variants: variant, Origin: "import"})
	save(models.Model{ID: "imp-missing", Tasks: []string{"chat"}, Variants: variant, Origin: "import"})
	save(models.Model{ID: "tiny-chat", Tasks: []string{"chat"}, Variants: variant, Origin: "import"})
	st.Close()
	for _, id := range []string{"imp-ready", "tiny-chat"} {
		d := acquire.ImportedDir(dir, id)
		os.MkdirAll(d, 0o700)
		os.WriteFile(filepath.Join(d, "w.gguf"), []byte("GGUF"), 0o600)
	}

	code, out := runApp(t, "--root", dir, "--json", "connect")
	if code != 0 {
		t.Fatalf("connect: %d %s", code, out)
	}
	var env struct {
		Data struct {
			Model  string   `json:"model"`
			Models []string `json:"models"`
		} `json:"data"`
	}
	if err := json.NewDecoder(strings.NewReader(out[strings.Index(out, "{"):])).Decode(&env); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	// The one behaviour change 5-7f makes here: a model whose files are all 0 bytes, with no expected
	// size, isn't installed (web's rule, which the command line now shares).
	if slices.Contains(env.Data.Models, "zero-chat") {
		t.Errorf("a model whose files are 0 bytes is listed as installed: %q", env.Data.Models)
	}
	got := "models: " + strings.Join(env.Data.Models, ", ") + "\ndefault: " + env.Data.Model + "\n"
	golden := filepath.Join("testdata", "connect_candidates.golden")
	if os.Getenv("LOCALBASE_UPDATE_CANDIDATES_GOLDEN") == "1" {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("differs from %s:\n%s", golden, got)
	}
}
