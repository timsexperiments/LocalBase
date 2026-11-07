package gateway

import (
	"encoding/json"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"net/http"
	"net/url"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/meta"
)

// GET /_localbase/models and /_localbase/models/{id}: catalog facts and
// observed device state (model-metadata.ts). It never starts runtimes,
// downloads or hashes files.

// modelRoutePermission is what a request under /_localbase/models/ needs. Changing a model's
// placement or unloading it needs models:manage and not models:read as well (DESIGN §20.2,
// §23); everything else here reads.
func modelRoutePermission(r *http.Request) string {
	path := r.URL.EscapedPath()
	switch {
	case strings.HasSuffix(path, "/unload") && r.Method == http.MethodPost,
		strings.HasSuffix(path, "/placement") && (r.Method == http.MethodPut || r.Method == http.MethodDelete):
		return "models:manage"
	}
	return "models:read"
}

func (s *Server) modelMetadata(w http.ResponseWriter, r *http.Request) {
	if raw, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.EscapedPath(), "/_localbase/models/"), "/placement"); ok && raw != "" {
		if id, err := url.PathUnescape(raw); err == nil {
			s.placementAPI(w, r, id)
			return
		}
	}
	if raw, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.EscapedPath(), "/_localbase/models/"), "/admission"); ok && raw != "" {
		if id, err := url.PathUnescape(raw); err == nil {
			s.admissionAPI(w, r, id)
			return
		}
	}
	if raw, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.EscapedPath(), "/_localbase/models/"), "/unload"); ok && raw != "" {
		if id, err := url.PathUnescape(raw); err == nil {
			s.unloadAPI(w, r, id)
			return
		}
	}
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, r, "GET")
		return
	}
	snap := s.LLM.Snapshot()
	voices, _ := s.Store.Voices(r.Context())
	in := meta.Input{Store: s.Store, RootDir: s.Root.Dir, Config: s.LLM.Config(), MemoryGB: s.MemoryGB, Voices: voices, Snapshot: &snap, Catalog: s.Catalog}
	if r.URL.Path != "/_localbase/models" {
		raw := strings.TrimPrefix(r.URL.EscapedPath(), "/_localbase/models/")
		id, err := url.PathUnescape(raw)
		if raw == "" || err != nil {
			RouteDisabled(w, r)
			return
		}
		spec := s.Catalog.ByID(id)
		if spec == nil {
			ModelNotFound(w, r, id)
			return
		}
		m, problems := meta.Project(r.Context(), in, spec)
		s.reportReads(w, problems)
		if len(problems) == 0 && (spec.Kind != "llm" || s.Placement == nil) {
			JSON(w, 200, m)
			return
		}
		// Go only (DESIGN §20.2): device.placement is the split the loaded
		// model runs with, or the next load's when it is not loaded.
		b, _ := json.Marshal(m)
		var out map[string]any
		json.Unmarshal(b, &out)
		if len(problems) > 0 {
			out["degraded"], out["readProblems"] = true, readProblemsJSON(problems)
		}
		if dev, ok := out["device"].(map[string]any); ok && spec.Kind == "llm" && s.Placement != nil {
			if plan, ok := s.LLM.LoadedPlacement(id); ok && plan.Placement != nil {
				dev["placement"] = placementJSON(plan)
			} else if next, err := s.LLM.PreviewPlacement(id, s.Placement.Request(id)); err == nil && next.Placement != nil {
				pj := placementJSON(next)
				delete(pj, "plannedAt")
				pj["preview"] = true
				dev["placement"] = pj
			}
		}
		JSON(w, 200, out)
		return
	}
	var data []meta.Model
	var problems []*store.ReadError
	for _, spec := range s.Catalog.All() {
		m, failed := meta.Project(r.Context(), in, spec)
		problems = append(problems, failed...)
		data = append(data, m)
	}
	// A filter on fit can't place a model whose figure couldn't be read, so a filtered list says which.
	var unknown []string
	if len(problems) > 0 {
		unfiltered := r.URL.Query()
		unfiltered.Del("fit")
		unknown = []string{}
		for _, m := range meta.Query(data, unfiltered) {
			if m.Catalog.Memory.Fit.Verdict == "unknown" {
				unknown = append(unknown, m.ID)
			}
		}
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
	out := map[string]any{"object": "list", "host": map[string]any{"memory": map[string]any{
		"kind": kind, "system": map[string]any{"capacityBytes": topo.SystemCapacity, "availableBytes": avail},
		"accelerators": []any{}}}, "data": data}
	if len(problems) > 0 {
		s.reportReads(w, problems)
		out["degraded"], out["readProblems"], out["unknown"] = true, readProblemsJSON(problems), unknown
	}
	JSON(w, 200, out)
}
