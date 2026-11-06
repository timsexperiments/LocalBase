package backup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A built-in entry to patch and copy, as a person's catalog.d would.
func builtInEntry(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "catalog", "data", "llm", "clef-q4_k_m.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func writeCatalogEntry(t *testing.T, rootDir string, entry map[string]any) {
	t.Helper()
	b, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootDir, "catalog.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(rootDir, "catalog.d", entry["modelId"].(string)+".json"), string(b))
}

// A backup's catalog.d is what the restored root's catalog will trust as its own, so the report names each
// entry, new or patched, what a patch changes, and where it downloads from.
func TestTheReportListsWhatTheCatalogWillTrust(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	writeCatalogEntry(t, r.Dir, map[string]any{"modelId": "clef-q4_k_m", "source": "https://huggingface.co/someone-else/Clef-GGUF"})
	added := builtInEntry(t)
	added["modelId"] = "a-new-model"
	files := added["artifacts"].([]any)
	first := files[0].(map[string]any)
	first["source"] = map[string]any{"repositoryUrl": "https://huggingface.co/elsewhere/repo", "revision": "0123456789abcdef0123456789abcdef01234567"}
	writeCatalogEntry(t, r.Dir, added)
	res := create(t, r, false)
	rep := stageFile(t, res.Output)
	var ids []string
	for _, e := range rep.Catalog {
		ids = append(ids, e.Model)
	}
	// my-model is the test root's own entry.
	if !slices.Equal(ids, []string{"a-new-model", "clef-q4_k_m", "my-model"}) {
		t.Fatalf("entries, by id: %v", ids)
	}
	if e := rep.Catalog[0]; !e.New || e.Source != added["source"] || e.Revision != added["repositoryRevision"] ||
		len(e.Files) != 1 || e.Files[0].File != first["filename"] || e.Files[0].From != "https://huggingface.co/elsewhere/repo" {
		t.Errorf("the new entry: %+v", e)
	}
	if e := rep.Catalog[1]; e.New || !slices.Equal(e.Changes, []string{"source"}) || e.Source != "https://huggingface.co/someone-else/Clef-GGUF" {
		t.Errorf("the patched entry: %+v", e)
	}
}

// A root with no catalog.d brings nothing for its catalog to trust, and a patch that changes nothing changes
// nothing.
func TestAReportWithoutCatalogEntriesSaysNothingOfThem(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	// The test root comes with an entry of its own.
	if err := os.RemoveAll(filepath.Join(r.Dir, "catalog.d")); err != nil {
		t.Fatal(err)
	}
	if rep := stageFile(t, create(t, r, false).Output); len(rep.Catalog) != 0 {
		t.Errorf("entries from a root with none: %+v", rep.Catalog)
	}
	same := builtInEntry(t)
	writeCatalogEntry(t, r.Dir, map[string]any{"modelId": "clef-q4_k_m", "source": same["source"]})
	if rep := stageFile(t, create(t, r, false).Output); len(rep.Catalog) != 0 {
		t.Errorf("a patch that changes nothing: %+v", rep.Catalog)
	}
}
