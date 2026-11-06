package backup

import (
	"slices"
	"sort"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/install"
)

// CatalogEntry is an entry of a backup's catalog.d. A root's catalog trusts those as the person's own, so a
// restore says what each adds or changes, and where it downloads from, before anything is changed.
type CatalogEntry struct {
	Model string `json:"model"`
	New   bool   `json:"new"`
	// Changes are the top-level fields it changes of the built-in entry.
	Changes  []string `json:"changes,omitempty"`
	Source   string   `json:"source"`
	Revision string   `json:"revision"`
	// WasSource and WasRevision are the built-in entry's, when a patch changes either.
	WasSource   string `json:"wasSource,omitempty"`
	WasRevision string `json:"wasRevision,omitempty"`
	// Downloads says it changes where something downloads from: it's new, or it patches its source, revision
	// or files. Those are never left out of the report's list.
	Downloads bool         `json:"changesDownloads"`
	Files     []FileSource `json:"files,omitempty"`
}

// FileSource is a file that downloads from somewhere other than its entry's own source would put it.
type FileSource struct {
	File string `json:"file"`
	From string `json:"from"`
}

// downloadFields are what decide where a model downloads from.
var downloadFields = []string{"source", "repositoryRevision", "artifacts"}

// catalogEntries lists what a staged catalog.d adds or changes: those that change where something downloads
// from first, by id, then the rest, by id. A patch that changes nothing is left out, since the entry stays as
// built in. A file's address is the installer's own, so the report can't say one place while it fetches from
// another.
func catalogEntries(cat *catalog.Catalog) ([]CatalogEntry, error) {
	builtIn, err := catalog.Load("")
	if err != nil {
		return nil, err
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
			if was := builtIn.ByID(s.ModelID); was != nil && (slices.Contains(s.PatchedFields, "source") || slices.Contains(s.PatchedFields, "repositoryRevision")) {
				e.WasSource, e.WasRevision = bounded(was.Source, 300), bounded(was.RepositoryRevision, 80)
			}
		}
		e.Downloads = e.New || slices.ContainsFunc(downloadFields, func(f string) bool { return slices.Contains(s.PatchedFields, f) })
		if e.New || slices.Contains(s.PatchedFields, "artifacts") {
			for _, a := range s.Artifacts {
				plain := a
				plain.Source = nil
				if got := install.DownloadURL(s, a); got != install.DownloadURL(s, plain) {
					e.Files = append(e.Files, FileSource{File: bounded(a.Filename, 120), From: bounded(got, 300)})
				}
			}
		}
		out = append(out, e)
	}
	orderCatalogEntries(out)
	return out, nil
}

// orderCatalogEntries puts those that change where something downloads from first, then the rest, each by
// id. The file chooses the ids, so harmless entries sorting first can't push the one that matters out of a
// list that's cut short.
func orderCatalogEntries(entries []CatalogEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Downloads != entries[j].Downloads {
			return entries[i].Downloads
		}
		return entries[i].Model < entries[j].Model
	})
}
