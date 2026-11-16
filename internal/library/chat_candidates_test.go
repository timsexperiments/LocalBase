package library

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/acquire"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Installed means every file at its expected size; a file with no expected size counts at its size,
// so a model whose files are all 0 bytes is not installed.
func TestInstalledBytes(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, n int) {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.gguf", 4)
	write("b.gguf", 2)
	write("zero.gguf", 0)
	spec := func(arts ...catalog.Artifact) *catalog.Spec { return &catalog.Spec{Artifacts: arts} }
	for name, tc := range map[string]struct {
		s    *catalog.Spec
		want int64
	}{
		"all there":          {spec(catalog.Artifact{Filename: "a.gguf", ExpectedSizeBytes: 4}, catalog.Artifact{Filename: "b.gguf"}), 6},
		"the wrong size":     {spec(catalog.Artifact{Filename: "a.gguf", ExpectedSizeBytes: 5}), 0},
		"one missing":        {spec(catalog.Artifact{Filename: "a.gguf"}, catalog.Artifact{Filename: "c.gguf"}), 0},
		"all 0 bytes":        {spec(catalog.Artifact{Filename: "zero.gguf"}), 0},
		"no files":           {spec(), 0},
		"a directory, named": {spec(catalog.Artifact{Filename: "."}), 0},
	} {
		if got := InstalledBytes(dir, tc.s); got != tc.want {
			t.Errorf("%s: %d, want %d", name, got, tc.want)
		}
	}
}

// The configuration's selected then active LLMs, chat ones only, then chat imports; ready by their
// files; each id once; no imports without a library.
func TestChatCandidates(t *testing.T) {
	r, err := root.Init(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cat, err := catalog.LoadRoot(r.Dir, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	var chat, other, embedding string
	for _, s := range cat.All() {
		switch {
		case s.Kind == "llm" && s.IsEmbedding() && embedding == "":
			embedding = s.ModelID
		case s.Kind == "llm" && !s.IsEmbedding() && !s.IsDecision() && chat == "":
			chat = s.ModelID
		case s.Kind == "llm" && !s.IsEmbedding() && !s.IsDecision() && other == "":
			other = s.ModelID
		}
	}
	if chat == "" || other == "" {
		t.Fatal("the built-in catalog has fewer than two chat LLMs")
	}
	// chat is installed (sparse files at their expected sizes); other isn't.
	llmDir := r.ModelsDir("llm")
	os.MkdirAll(llmDir, 0o700)
	for _, a := range cat.ByID(chat).Artifacts {
		f, err := os.Create(filepath.Join(llmDir, a.Filename))
		if err != nil {
			t.Fatal(err)
		}
		f.Truncate(a.ExpectedSizeBytes)
		f.Close()
	}
	save := func(m models.Model) {
		b, _ := json.Marshal(m)
		if err := st.SaveImportedModel(t.Context(), m.ID, b); err != nil {
			t.Fatal(err)
		}
	}
	variant := []models.Variant{{ID: "q4", Artifacts: []models.Artifact{{Filename: "w.gguf", Role: "weights", Required: true}}}}
	save(models.Model{ID: "imp-ready", Tasks: []string{"chat"}, Variants: variant, Origin: "import"})
	save(models.Model{ID: "imp-embed", Tasks: []string{"embeddings"}, Variants: variant, Origin: "import"})
	save(models.Model{ID: chat, Tasks: []string{"chat"}, Variants: variant, Origin: "import"})
	for _, id := range []string{"imp-ready", chat} {
		d := acquire.ImportedDir(r.Dir, id)
		os.MkdirAll(d, 0o700)
		os.WriteFile(filepath.Join(d, "w.gguf"), []byte("GGUF"), 0o600)
	}
	lib, err := Open(r, st, cat, nil)
	if err != nil {
		t.Fatal(err)
	}
	selected := []string{other, chat}
	if embedding != "" {
		selected = append(selected, embedding)
	}
	cfg := &store.Config{Selected: map[string][]string{"llm": selected}, Active: map[string]string{"llm": chat}}
	got := ChatCandidates(t.Context(), cfg, cat, llmDir, lib)
	var ids []string
	ready := map[string]bool{}
	for _, c := range got {
		ids = append(ids, c.ID)
		ready[c.ID] = c.Ready
		if (c.Spec == nil) == (c.Entry == nil) {
			t.Errorf("%s: spec %v, entry %v", c.ID, c.Spec != nil, c.Entry != nil)
		}
	}
	if !slices.Equal(ids, []string{other, chat, "imp-ready"}) {
		t.Errorf("listed %q", ids)
	}
	if ready[other] || !ready[chat] || !ready["imp-ready"] {
		t.Errorf("ready %v", ready)
	}
	noLib := ChatCandidates(t.Context(), cfg, cat, llmDir, nil)
	if len(noLib) != 2 || noLib[0].ID != other || noLib[1].ID != chat {
		t.Errorf("without a library: %+v", noLib)
	}
	cfg.Selected["llm"] = nil
	if got := ChatCandidates(t.Context(), cfg, cat, llmDir, nil); len(got) != 1 || got[0].ID != chat {
		t.Errorf("active only: %+v", got)
	}
}
