package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

// communityEntry is a feed entry for a model whose main file has this name and checksum, and whose
// other files have the names given.
func communityEntry(t *testing.T, id, kind, filename, sha string, others ...string) json.RawMessage {
	t.Helper()
	artifacts := []any{map[string]any{"sourcePath": filename, "filename": filename, "expectedSizeBytes": 4096, "sha256": sha, "role": "primary"}}
	for _, other := range others {
		artifacts = append(artifacts, map[string]any{"sourcePath": other, "filename": other, "expectedSizeBytes": 4096,
			"sha256": strings.Repeat("e", 64), "role": "supplementary"})
	}
	b, err := json.Marshal(map[string]any{
		"modelId": id, "displayName": id, "kind": kind, "provider": "Community", "family": "Test",
		"version": "test", "size": "tiny", "quant": "Q8_0", "storageGb": 0.01,
		"source": "https://huggingface.co/community/tiny", "repositoryRevision": strings.Repeat("b", 40),
		"artifacts":       artifacts,
		"inputModalities": []string{"audio"}, "outputModalities": []string{"text"}, "capabilities": []string{"speech-to-text"},
		"commercialStatus": "open", "catch": "Community entry.", "notes": "From the test feed.",
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func loadFeed(t *testing.T, entries ...json.RawMessage) *Catalog {
	t.Helper()
	c, err := LoadLayers("", &RemoteLayer{URL: "feed", Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A built-in speech model, to collide with.
func builtInFile(t *testing.T) (kind string, a Artifact) {
	t.Helper()
	c, err := LoadLayers("", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range c.Models {
		if s.Kind == "stt" {
			return s.Kind, s.Artifacts[0]
		}
	}
	t.Fatal("no built-in speech model")
	return "", Artifact{}
}

func refusedForItsFile(c *Catalog, id string) bool {
	if c.ByID(id) != nil {
		return false
	}
	for _, d := range c.Diagnostics {
		if d.ModelID == id && strings.Contains(d.Message, "file") {
			return true
		}
	}
	return false
}

// Installing an entry replaces whatever file has its name, and every model of a kind shares one
// directory, so a feed entry naming a qualified model's file could take that file's place.
func TestACommunityEntryCannotTakeABuiltInModelsFile(t *testing.T) {
	kind, a := builtInFile(t)
	other := strings.Repeat("c", 64)
	c := loadFeed(t,
		communityEntry(t, "takes-the-file", kind, a.Filename, other),
		communityEntry(t, "in-another-case", kind, strings.ToUpper(a.Filename), other),
		communityEntry(t, "its-own-file", kind, "ggml-community-own.bin", other),
	)
	for _, id := range []string{"takes-the-file", "in-another-case"} {
		if !refusedForItsFile(c, id) {
			t.Errorf("%s was accepted, or refused with no word why: %v", id, c.Diagnostics)
		}
	}
	if c.ByID("its-own-file") == nil {
		t.Errorf("an entry with its own file was refused: %v", c.Diagnostics)
	}
}

// The same bytes under the same name take nothing from anyone.
func TestACommunityEntryMayShareAFileThatIsTheSame(t *testing.T) {
	kind, a := builtInFile(t)
	c := loadFeed(t, communityEntry(t, "same-bytes", kind, a.Filename, a.SHA256))
	if c.ByID("same-bytes") == nil {
		t.Errorf("it was refused: %v", c.Diagnostics)
	}
}

// Each kind has its own directory, so the same name under another kind is another file.
func TestAFileNameInAnotherKindIsAnotherFile(t *testing.T) {
	_, a := builtInFile(t)
	c := loadFeed(t, communityEntry(t, "other-kind", "tts", a.Filename, strings.Repeat("c", 64)))
	if refusedForItsFile(c, "other-kind") {
		t.Errorf("it was refused for its file: %v", c.Diagnostics)
	}
}

// Between two community entries, the first keeps the name.
func TestTwoCommunityEntriesCannotShareAName(t *testing.T) {
	c := loadFeed(t,
		communityEntry(t, "first", "stt", "ggml-shared-name.bin", strings.Repeat("c", 64)),
		communityEntry(t, "second", "stt", "ggml-shared-name.bin", strings.Repeat("d", 64)),
	)
	if c.ByID("first") == nil {
		t.Errorf("the first was refused: %v", c.Diagnostics)
	}
	if !refusedForItsFile(c, "second") {
		t.Errorf("the second was accepted: %v", c.Diagnostics)
	}
}

// An entry with no checksum can't be shown to be the same file.
func TestAnEntryWithNoChecksumIsNotTheSameFile(t *testing.T) {
	kind, a := builtInFile(t)
	c := loadFeed(t, communityEntry(t, "no-sum", kind, a.Filename, ""))
	if c.ByID("no-sum") != nil {
		t.Errorf("it was accepted: %v", c.Diagnostics)
	}
}

// It isn't only an entry's main file that gets replaced.
func TestEveryFileOfAnEntryIsLookedAt(t *testing.T) {
	kind, a := builtInFile(t)
	c := loadFeed(t,
		communityEntry(t, "second-file-taken", kind, "ggml-community-main.bin", strings.Repeat("c", 64), a.Filename),
		communityEntry(t, "second-file-free", kind, "ggml-community-other.bin", strings.Repeat("c", 64), "ggml-community-extra.bin"),
	)
	if !refusedForItsFile(c, "second-file-taken") {
		t.Errorf("it was accepted: %v", c.Diagnostics)
	}
	if c.ByID("second-file-free") == nil {
		t.Errorf("an entry with two files of its own was refused: %v", c.Diagnostics)
	}
}

// A file named only by a refused entry stays free for the next one.
func TestARefusedEntryClaimsNothing(t *testing.T) {
	kind, a := builtInFile(t)
	c := loadFeed(t,
		communityEntry(t, "refused", kind, "ggml-community-free.bin", strings.Repeat("c", 64), a.Filename),
		communityEntry(t, "next", kind, "ggml-community-free.bin", strings.Repeat("d", 64)),
	)
	if c.ByID("next") == nil {
		t.Errorf("the next was refused: %v", c.Diagnostics)
	}
}

func TestTwoEntriesWithNoChecksumAreNotTheSameFile(t *testing.T) {
	c := loadFeed(t,
		communityEntry(t, "first", "stt", "ggml-shared-name.bin", ""),
		communityEntry(t, "second", "stt", "ggml-shared-name.bin", ""),
	)
	if c.ByID("first") == nil || !refusedForItsFile(c, "second") {
		t.Errorf("the first: %v; the second refused: %v; %v", c.ByID("first") != nil, refusedForItsFile(c, "second"), c.Diagnostics)
	}
}

// What's left of the catalog is the same whichever way it's asked.
func TestARefusedEntryIsGoneFromTheList(t *testing.T) {
	kind, a := builtInFile(t)
	c := loadFeed(t, communityEntry(t, "takes-the-file", kind, a.Filename, strings.Repeat("c", 64)))
	for _, s := range c.Models {
		if s.ModelID == "takes-the-file" {
			t.Error("it's still listed")
		}
	}
}
