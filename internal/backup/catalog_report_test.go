package backup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	wantFrom := "https://huggingface.co/elsewhere/repo/resolve/0123456789abcdef0123456789abcdef01234567/" + first["sourcePath"].(string)
	if e := rep.Catalog[0]; !e.New || !e.Downloads || e.Source != added["source"] || e.Revision != added["repositoryRevision"] ||
		len(e.Files) != 1 || e.Files[0].File != first["filename"] || e.Files[0].From != wantFrom {
		t.Errorf("the new entry: %+v", e)
	}
	builtIn := builtInEntry(t)
	if e := rep.Catalog[1]; e.New || !e.Downloads || !slices.Equal(e.Changes, []string{"source"}) || e.Source != "https://huggingface.co/someone-else/Clef-GGUF" ||
		e.WasSource != builtIn["source"] || e.WasRevision != builtIn["repositoryRevision"] {
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

// A file's own source is read by the installer with Go's JSON rules, which match keys whatever their case and
// keep the last one. The report shows the address the installer would use, so a file can't show one place and
// fetch from another.
func TestAFileShowsWhereTheInstallerWouldFetchIt(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	added := builtInEntry(t)
	added["modelId"] = "a-new-model"
	first := added["artifacts"].([]any)[0].(map[string]any)
	first["source"] = json.RawMessage(`{"repositoryUrl":"https://huggingface.co/familiar/repo","RepositoryURL":"https://huggingface.co/elsewhere/repo","revision":"0123456789abcdef0123456789abcdef01234567"}`)
	writeCatalogEntry(t, r.Dir, added)
	rep := stageFile(t, create(t, r, false).Output)
	for _, e := range rep.Catalog {
		if e.Model != "a-new-model" {
			continue
		}
		if len(e.Files) != 1 || !strings.Contains(e.Files[0].From, "huggingface.co/elsewhere/repo/") {
			t.Errorf("the report says the file comes from %+v, and the installer fetches it from elsewhere/repo", e.Files)
		}
		return
	}
	t.Fatal("the entry isn't in the report")
}

// Entries that change where something downloads from come before the rest, whatever their ids.
func TestEntriesThatChangeDownloadsComeFirst(t *testing.T) {
	var entries []CatalogEntry
	for i := range 30 {
		entries = append(entries, CatalogEntry{Model: fmt.Sprintf("aa-%02d", i), Changes: []string{"notes"}})
	}
	entries = append(entries, CatalogEntry{Model: "zz-hostile", New: true, Downloads: true}, CatalogEntry{Model: "mm-patched", Downloads: true})
	orderCatalogEntries(entries)
	if entries[0].Model != "mm-patched" || entries[1].Model != "zz-hostile" || entries[2].Model != "aa-00" {
		t.Errorf("the order: %s, %s, %s", entries[0].Model, entries[1].Model, entries[2].Model)
	}
}
