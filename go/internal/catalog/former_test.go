package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
)

// Former built-in entries (#368), after TS src/catalog/former-adoption.test.ts.

const (
	installedFormer = "qwen2.5-coder-1.5b-instruct-q4_k_m"
	selectedFormer  = "mistral-nemo-12b-instruct-q4_k_m" // measured
	unusedFormer    = "gemma-3-27b-it-q4_k_m"
	promotedID      = "qwen3.6-35b-a3b-iq4_nl_xl" // added from Hugging Face before it joined the catalog
)

func formerData(t *testing.T, id string) []byte {
	t.Helper()
	for _, e := range formerOnce() {
		if e.spec.ModelID == id {
			return e.data
		}
	}
	t.Fatalf("no former entry %s", id)
	return nil
}

func sparseInstall(t *testing.T, root string, s *Spec) {
	t.Helper()
	dir := filepath.Join(root, "models", s.Kind)
	os.MkdirAll(dir, 0o700)
	for _, a := range s.Artifacts {
		f, err := os.Create(filepath.Join(dir, a.Filename))
		if err != nil {
			t.Fatal(err)
		}
		f.Truncate(a.ExpectedSizeBytes)
		f.Close()
	}
}

func loadRoot(t *testing.T, root string) *Catalog {
	t.Helper()
	c, err := LoadRoot(root, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestFormerIndex(t *testing.T) {
	built, _ := Load("")
	if len(FormerIDs()) == 0 {
		t.Fatal("no former entries")
	}
	for _, id := range FormerIDs() {
		if built.ByID(id) != nil {
			t.Errorf("%s is both built-in and former", id)
		}
	}
}

func TestFormerEntriesStayOutOfNewInstalls(t *testing.T) {
	root := t.TempDir()
	c := loadRoot(t, root)
	for _, id := range []string{installedFormer, unusedFormer} {
		if c.ByID(id) != nil {
			t.Errorf("%s is offered on a new install", id)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "catalog.d")); err == nil {
		t.Error("catalog.d was created on a new install")
	}
}

func TestInstalledFormerEntryBecomesUserEntry(t *testing.T) {
	root := t.TempDir()
	sparseInstall(t, root, FormerEntry(installedFormer))
	c := loadRoot(t, root)
	kept := c.ByID(installedFormer)
	if kept == nil {
		t.Fatal("not kept")
	}
	if c.ByID(unusedFormer) != nil {
		t.Error("an unused former entry was kept")
	}
	if kept.Trust != "user" || kept.Origin != "user" || kept.Tier != "user" {
		t.Errorf("provenance %s/%s/%s", kept.Trust, kept.Origin, kept.Tier)
	}
	if !strings.HasPrefix(kept.Notes, FormerEntryHint) {
		t.Errorf("notes %q", kept.Notes)
	}
	// The file is the shipped data, except for the hint in front of its notes.
	file, _ := os.ReadFile(filepath.Join(root, "catalog.d", installedFormer+".json"))
	fv, _ := jsjson.Parse(file)
	dv, _ := jsjson.Parse(formerData(t, installedFormer))
	notes, _ := dv.(*jsjson.Object).Get("notes")
	fv.(*jsjson.Object).Set("notes", notes)
	if string(jsjson.Stringify(fv)) != string(jsjson.Stringify(dv)) {
		t.Errorf("kept file differs from the shipped data:\n%s\n%s", jsjson.Stringify(fv), jsjson.Stringify(dv))
	}
	if got := AdoptFormerEntries(root, nil); len(got) != 0 {
		t.Errorf("adopted again: %v", got)
	}
}

func TestKeptEntryKeepsMeasurementsUntilModified(t *testing.T) {
	root := t.TempDir()
	sparseInstall(t, root, FormerEntry(selectedFormer))
	if !loadRoot(t, root).ByID(selectedFormer).UsesReferenceMeasurements() {
		t.Fatal("a kept entry lost its measurements")
	}
	path := filepath.Join(root, "catalog.d", selectedFormer+".json")
	b, _ := os.ReadFile(path)
	v, _ := jsjson.Parse(b)
	v.(*jsjson.Object).Set("contextWindowTokens", float64(4096))
	os.WriteFile(path, jsjson.Stringify(v), 0o600)
	if loadRoot(t, root).ByID(selectedFormer).UsesReferenceMeasurements() {
		t.Error("a modified entry still uses the measurements")
	}
}

func TestUserPatchOfFormerEntryAppliesOnTop(t *testing.T) {
	root := t.TempDir()
	sparseInstall(t, root, FormerEntry(installedFormer))
	os.MkdirAll(filepath.Join(root, "catalog.d"), 0o700)
	os.WriteFile(filepath.Join(root, "catalog.d", installedFormer+".json"),
		[]byte(`{"modelId":"`+installedFormer+`","displayName":"My coder"}`), 0o600)
	kept := loadRoot(t, root).ByID(installedFormer)
	if kept == nil || kept.DisplayName != "My coder" || len(kept.Artifacts) != len(FormerEntry(installedFormer).Artifacts) {
		t.Fatalf("kept %+v", kept)
	}
}

func TestRemovedKeptEntryIsNotWrittenAgain(t *testing.T) {
	root := t.TempDir()
	sparseInstall(t, root, FormerEntry(installedFormer))
	c := loadRoot(t, root)
	if p, err := RemoveUserEntry(root, c, installedFormer); err != nil || p == "" {
		t.Fatalf("remove: %q %v", p, err)
	}
	if loadRoot(t, root).ByID(installedFormer) != nil {
		t.Error("written again after removal")
	}
	b, _ := os.ReadFile(FormerAdoptionLogPath(root))
	if !strings.Contains(string(b), installedFormer) {
		t.Errorf("adoption log %q", b)
	}
}

func TestReferencedFormerEntriesAreKept(t *testing.T) {
	root := t.TempDir()
	got := AdoptFormerEntries(root, MissingFormerIDs([]string{selectedFormer, "", selectedFormer, "qwen3.5-4b-q4_k_m"}, func(string) bool { return false }))
	if !slices.Equal(got, []string{selectedFormer}) {
		t.Fatalf("adopted %v", got)
	}
	if loadRoot(t, root).ByID(selectedFormer) == nil {
		t.Error("not in the catalog")
	}
}

// addedBeforeBuiltIn is a complete user spec for the promoted id that pins
// the built-in's files, with its own description (TS ADDED_BEFORE_BUILT_IN).
func addedBeforeBuiltIn(t *testing.T, edit func(map[string]any)) []byte {
	t.Helper()
	b, err := builtInFS.ReadFile("data/llm/" + promotedID + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	m["catch"] = "Added from Hugging Face."
	m["notes"] = "My entry."
	if edit != nil {
		edit(m)
	}
	out, _ := json.MarshalIndent(m, "", "  ")
	return out
}

func TestUserFileThatPinsBuiltInFilesIsSetAside(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "catalog.d"), 0o700)
	path := filepath.Join(root, "catalog.d", promotedID+".json")
	os.WriteFile(path, addedBeforeBuiltIn(t, func(m map[string]any) {
		m["source"] = m["source"].(string) + "/" // trailing slashes do not matter
	}), 0o600)
	s := loadRoot(t, root).ByID(promotedID)
	built, _ := Load("")
	if s.Trust != "qualified" || s.Tier != "built-in" || len(s.PatchedFields) != 0 || s.Catch != built.ByID(promotedID).Catch {
		t.Errorf("trust %s tier %s patched %v catch %q", s.Trust, s.Tier, s.PatchedFields, s.Catch)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("the user file was removed")
	}
}

func TestUserFileThatPinsOtherFilesStaysAPatch(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "catalog.d"), 0o700)
	os.WriteFile(filepath.Join(root, "catalog.d", promotedID+".json"), addedBeforeBuiltIn(t, func(m map[string]any) {
		arts := m["artifacts"].([]any)
		arts[0].(map[string]any)["sha256"] = strings.Repeat("b", 64)
	}), 0o600)
	s := loadRoot(t, root).ByID(promotedID)
	if s.Trust != "modified" || s.Tier != "user" || s.Catch != "Added from Hugging Face." {
		t.Errorf("trust %s tier %s catch %q", s.Trust, s.Tier, s.Catch)
	}
}

func TestFeedIgnoresFormerIDs(t *testing.T) {
	if !builtInIDs[installedFormer] || !builtInIDs["qwen3.5-4b-q4_k_m"] {
		t.Error("the feed may provide a former or built-in id")
	}
}
