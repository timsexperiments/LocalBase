package backup

import (
	"encoding/json"
	"slices"
	"sort"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
)

// CatalogEntry is an entry of a backup's catalog.d. A root's catalog trusts those as the person's own, so a
// restore says what each adds or changes, and where it downloads from, before anything is changed.
type CatalogEntry struct {
	Model string `json:"model"`
	New   bool   `json:"new"`
	// Changes are the top-level fields it changes of the built-in entry.
	Changes  []string     `json:"changes,omitempty"`
	Source   string       `json:"source"`
	Revision string       `json:"revision"`
	Files    []FileSource `json:"files,omitempty"`
}

// FileSource is a file of an entry that comes from somewhere other than the entry's own source.
type FileSource struct {
	File string `json:"file"`
	From string `json:"from"`
}

// catalogEntries lists what a staged catalog.d adds or changes, by id. A patch that changes nothing is left
// out: the entry stays as built in.
func catalogEntries(dir string) ([]CatalogEntry, error) {
	cat, err := catalog.LoadRoot(dir, nil)
	if err != nil {
		return nil, refuse("The catalog in this backup is not valid: %v", err)
	}
	var out []CatalogEntry
	for _, s := range cat.Models {
		if s.Tier != "user" {
			continue
		}
		e := CatalogEntry{Model: bounded(s.ModelID, 120), New: s.Origin == "user", Source: bounded(s.Source, 300), Revision: bounded(s.RepositoryRevision, 80)}
		if !e.New {
			if len(s.PatchedFields) == 0 {
				continue
			}
			e.Changes = append([]string{}, s.PatchedFields...)
		}
		// A file has a source of its own only where the entry's files are this backup's.
		if e.New || slices.Contains(s.PatchedFields, "artifacts") {
			for _, a := range s.Artifacts {
				if from := artifactFrom(a.Source); from != "" {
					e.Files = append(e.Files, FileSource{File: bounded(a.Filename, 120), From: bounded(from, 300)})
				}
			}
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out, nil
}

// artifactFrom is where a file's own source points: a repository, a URL or a path, or the source as written
// when it's none of those.
func artifactFrom(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var src map[string]any
	if json.Unmarshal(raw, &src) != nil {
		return string(raw)
	}
	for _, k := range []string{"repositoryUrl", "url", "path"} {
		if v, ok := src[k].(string); ok && v != "" {
			return v
		}
	}
	return string(raw)
}
