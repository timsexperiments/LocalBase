package web

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/acquire"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/library"
	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Imported chat models (M3.5) join the picker: ready when a variant has all
// its files, with vision from their capabilities. Other imports stay out.
func TestChatOffersImportedModels(t *testing.T) {
	r, err := root.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cat, err := catalog.LoadRoot(r.Dir, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	lib, err := library.Open(r, st, cat, nil)
	if err != nil {
		t.Fatal(err)
	}
	save := func(m models.Model) {
		b, _ := json.Marshal(m)
		if err := st.SaveImportedModel(t.Context(), m.ID, b); err != nil {
			t.Fatal(err)
		}
	}
	variant := []models.Variant{{ID: "q4", Artifacts: []models.Artifact{{Filename: "w.gguf", Role: "weights", Required: true}}}}
	save(models.Model{ID: "my-vision-chat", Name: "My Vision Chat", Tasks: []string{"chat"}, Capabilities: []string{"vision"}, Variants: variant, Origin: "import"})
	save(models.Model{ID: "my-missing-chat", Tasks: []string{"chat"}, Variants: variant, Origin: "import"})
	save(models.Model{ID: "my-embedder", Tasks: []string{"embeddings"}, Variants: variant, Origin: "import"})
	dir := acquire.ImportedDir(r.Dir, "my-vision-chat")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "w.gguf"), []byte("GGUF"), 0o644)

	u := New(Deps{NoAuth: true, Library: lib, Catalog: cat, Store: st, RootDir: r.Dir})
	got := map[string]chatModel{}
	for _, m := range u.importedChatModels(httptest.NewRequest("GET", "/app/chat", nil)) {
		got[m.ID] = m
	}
	if m := got["my-vision-chat"]; !m.Ready || !m.Vision || m.Name != "My Vision Chat" || strings.Join(m.Inputs, ",") != "text,image" {
		t.Errorf("vision import: %+v", m)
	}
	if m, ok := got["my-missing-chat"]; !ok || m.Ready || m.Name != "my-missing-chat" {
		t.Errorf("import without files: %+v", m)
	}
	if _, ok := got["my-embedder"]; ok {
		t.Error("an embedding import is offered for chat")
	}
}
