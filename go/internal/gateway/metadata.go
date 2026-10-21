package gateway

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/meta"
)

// GET /_localbase/models and /_localbase/models/{id}: catalog facts and
// observed device state (model-metadata.ts). It never starts runtimes,
// downloads or hashes files.

func (s *Server) modelMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r, "GET")
		return
	}
	snap := s.LLM.Snapshot()
	voices, _ := s.Store.Voices(r.Context())
	in := meta.Input{Store: s.Store, RootDir: s.Root.Dir, Config: s.LLM.Config(), MemoryGB: s.MemoryGB, Voices: voices, Snapshot: &snap, Catalog: s.Catalog}
	if r.URL.Path != "/_localbase/models" {
		raw := strings.TrimPrefix(r.URL.EscapedPath(), "/_localbase/models/")
		id, err := url.PathUnescape(raw)
		if raw == "" || err != nil {
			routeDisabled(w, r)
			return
		}
		spec := s.Catalog.ByID(id)
		if spec == nil {
			modelNotFound(w, r, id)
			return
		}
		writeJSON(w, 200, meta.Project(r.Context(), in, spec))
		return
	}
	var data []meta.Model
	for _, spec := range s.Catalog.All() {
		data = append(data, meta.Project(r.Context(), in, spec))
	}
	data = meta.Query(data, r.URL.Query())
	topo := s.Memory.Topology()
	var avail any
	if hs := s.Memory.Snapshot(r.Context()); hs.Available > 0 {
		avail = hs.Available
	}
	kind := "unified"
	if !topo.Unified {
		kind = "discrete"
	}
	if data == nil {
		data = []meta.Model{}
	}
	writeJSON(w, 200, map[string]any{"object": "list", "host": map[string]any{"memory": map[string]any{
		"kind": kind, "system": map[string]any{"capacityBytes": topo.SystemCapacity, "availableBytes": avail},
		"accelerators": []any{}}}, "data": data})
}
