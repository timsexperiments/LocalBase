package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/acquire"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
	"github.com/timsexperiments/LocalBase/go/internal/library"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

type candidatesHost struct{}

func (candidatesHost) Topology() memory.Topology {
	return memory.Topology{SystemCapacity: 64 * memory.GiB, Unified: true}
}
func (candidatesHost) Snapshot(context.Context) memory.Snapshot {
	return memory.Snapshot{Available: 48 * memory.GiB, Pressure: "normal"}
}

// chatCandidatesRoot is a root with two user catalog LLMs (tiny-chat, whose file is there, and
// zero-chat, whose one file is there at 0 bytes and expects no size), a built-in one not installed,
// and three imported chat models: one with its file, one without, and one that has tiny-chat's id.
func chatCandidatesRoot(t *testing.T) (*root.Root, *store.Store, *catalog.Catalog) {
	t.Helper()
	r, err := root.Init(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	b, err := os.ReadFile(filepath.Join("..", "catalog", "data", "llm", "qwen3.5-4b-q4_k_m.json"))
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
		if _, _, err := catalog.WriteUserEntry(r.Dir, o); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	llmDir := r.ModelsDir("llm")
	os.MkdirAll(llmDir, 0o700)
	os.WriteFile(filepath.Join(llmDir, "tiny-chat.gguf"), []byte("GGUF"), 0o600)
	os.WriteFile(filepath.Join(llmDir, "zero-chat.gguf"), nil, 0o600)
	cat, err := catalog.LoadRoot(r.Dir, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Diagnostics) > 0 {
		t.Fatalf("catalog: %+v", cat.Diagnostics)
	}
	save := func(m models.Model) {
		b, _ := json.Marshal(m)
		if err := st.SaveImportedModel(t.Context(), m.ID, b); err != nil {
			t.Fatal(err)
		}
	}
	variant := func(file string) []models.Variant {
		return []models.Variant{{ID: "q4", Artifacts: []models.Artifact{{Filename: file, Role: "weights", Required: true}}}}
	}
	save(models.Model{ID: "imp-ready", Tasks: []string{"chat"}, Variants: variant("w.gguf"), Origin: "import"})
	save(models.Model{ID: "imp-missing", Tasks: []string{"chat"}, Variants: variant("w.gguf"), Origin: "import"})
	save(models.Model{ID: "tiny-chat", Tasks: []string{"chat"}, Variants: variant("w.gguf"), Origin: "import"})
	for _, id := range []string{"imp-ready", "tiny-chat"} {
		dir := acquire.ImportedDir(r.Dir, id)
		os.MkdirAll(dir, 0o700)
		os.WriteFile(filepath.Join(dir, "w.gguf"), []byte("GGUF"), 0o600)
	}
	return r, st, cat
}

// The chat picker's and the connect page's models, recorded before their candidate rule moved to
// library (5-7f), for the root above: each picker entry's id, readiness and default flag in order,
// and the connect page's ids. LOCALBASE_UPDATE_CANDIDATES_GOLDEN=1 rewrites
// testdata/chat_candidates.golden.
func TestChatCandidatesGolden(t *testing.T) {
	r, st, cat := chatCandidatesRoot(t)
	cfg, err := st.LoadConfig(t.Context())
	if err != nil {
		cfg = &store.Config{Root: r.Dir}
	}
	cfg.Selected = map[string][]string{"llm": {"zero-chat", "qwen3.5-4b-q4_k_m", "tiny-chat"}}
	cfg.Active = map[string]string{"llm": "tiny-chat"}
	lib, err := library.Open(r, st, cat, nil)
	if err != nil {
		t.Fatal(err)
	}
	mem := memory.NewController(candidatesHost{}, memory.Reserve{}, false)
	llm := runtime.NewLLM(runtime.LLMDeps{Root: r.Dir, ModelsDir: r.ModelsDir("llm"), RuntimesDir: r.RuntimesDir(), Store: st, Catalog: cat,
		Memory: mem, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), MemoryGB: 64, Gate: lib.Gate()}, cfg)
	defer llm.Close()
	u := New(Deps{NoAuth: true, Library: lib, Catalog: cat, Store: st, RootDir: r.Dir, LLM: llm})
	req := httptest.NewRequest("GET", "/app/chat", nil)
	var b strings.Builder
	b.WriteString("chat picker:\n")
	for _, m := range u.chatModels(req) {
		b.WriteString("  " + m.ID)
		if m.Ready {
			b.WriteString(" ready")
		}
		if m.Default {
			b.WriteString(" default")
		}
		b.WriteString("\n")
	}
	b.WriteString("connect page:\n")
	for _, m := range u.connectModels(req) {
		b.WriteString("  " + m.ID + "\n")
	}
	got := b.String()
	golden := filepath.Join("testdata", "chat_candidates.golden")
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
