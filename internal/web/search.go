package web

import "net/http"

// Search in Studio (DESIGN §35). The page reads a person's text into vectors through
// /app/api/v1/embeddings and ranks it in the browser (search.js), so the server keeps nothing and
// there's no route of its own here. This only says which models the page may offer.

// searchModels are what Search can read text with (§35.1): selected catalog models that make
// embeddings and are installed, then imported models whose task is embeddings and whose files are
// all there. The API answers only for those, so nothing else is offered.
func (u *UI) searchModels(r *http.Request) []labModel {
	var out []labModel
	for _, m := range u.installedOnly(u.labModels("llm")) {
		if s := u.d.Catalog.ByID(m.ID); s != nil && s.IsEmbedding() {
			out = append(out, m)
		}
	}
	if u.d.Library == nil {
		return out
	}
	entries, err := u.d.Library.Entries(r.Context())
	if err != nil {
		return out
	}
	for i := range entries {
		e := &entries[i]
		if !e.Imported || !e.Model.HasTask("embeddings") {
			continue
		}
		ready := false
		for _, in := range u.d.Library.Inspect(e) {
			if in.Complete() {
				ready = true
				break
			}
		}
		if !ready {
			continue
		}
		name := e.Model.Name
		if name == "" {
			name = e.Model.ID
		}
		out = append(out, labModel{ID: e.Model.ID, Name: name})
	}
	return out
}
