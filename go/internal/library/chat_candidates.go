package library

import (
	"context"
	"os"
	"path/filepath"
	"slices"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// InstalledBytes returns the bytes on disk of s's files in modelsDir when every artifact is present at
// its expected size, else 0. It never hashes files.
func InstalledBytes(modelsDir string, s *catalog.Spec) int64 {
	var total int64
	for _, a := range s.Artifacts {
		info, err := os.Stat(filepath.Join(modelsDir, a.Filename))
		if err != nil || !info.Mode().IsRegular() || (a.ExpectedSizeBytes > 0 && info.Size() != a.ExpectedSizeBytes) {
			return 0
		}
		total += info.Size()
	}
	return total
}

// ChatCandidate is a model the chat picker and `connect` offer for chat.
type ChatCandidate struct {
	ID    string
	Spec  *catalog.Spec // a catalog model; nil for an import
	Entry *Entry        // an import; nil for a catalog model
	Ready bool          // every file it needs is here
}

// ChatCandidates are the chat models the page and the command line offer: the configuration's
// selected LLMs and then its active one if not selected, leaving out embedding and decision models,
// then the imported models with the chat task. Each id is listed once: an import with a catalog
// candidate's id is left out. A catalog model is ready when InstalledBytes says so, in llmDir; an
// import when one of its variants has all its files. With no lib there are no imports.
func ChatCandidates(ctx context.Context, cfg *store.Config, cat *catalog.Catalog, llmDir string, lib *Library) []ChatCandidate {
	var out []ChatCandidate
	listed := map[string]bool{}
	ids := slices.Clone(cfg.Selected["llm"])
	if a := cfg.Active["llm"]; a != "" && !slices.Contains(ids, a) {
		ids = append(ids, a)
	}
	for _, id := range ids {
		s := cat.ByID(id)
		if s == nil || s.Kind != "llm" || s.IsEmbedding() || s.IsDecision() {
			continue
		}
		out = append(out, ChatCandidate{ID: id, Spec: s, Ready: InstalledBytes(llmDir, s) > 0})
		listed[id] = true
	}
	if lib == nil {
		return out
	}
	entries, err := lib.Entries(ctx)
	if err != nil {
		return out
	}
	for i := range entries {
		e := &entries[i]
		if !e.Imported || !e.Model.HasTask("chat") || listed[e.Model.ID] {
			continue
		}
		c := ChatCandidate{ID: e.Model.ID, Entry: e}
		for _, in := range lib.Inspect(e) {
			if in.Complete() {
				c.Ready = true
				break
			}
		}
		out = append(out, c)
		listed[e.Model.ID] = true
	}
	return out
}
