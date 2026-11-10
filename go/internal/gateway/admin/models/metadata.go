package models

import (
	"encoding/json"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"net/http"
	"net/url"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
	"github.com/timsexperiments/LocalBase/go/internal/meta"
)

// GET /_localbase/models and /_localbase/models/{id}: catalog facts and
// observed device state (model-metadata.ts). It never starts runtimes,
// downloads or hashes files.

func (h *Handler) ModelMetadata(w http.ResponseWriter, r *http.Request) {
	if raw, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.EscapedPath(), "/_localbase/models/"), "/placement"); ok && raw != "" {
		if id, err := url.PathUnescape(raw); err == nil {
			h.placementAPI(w, r, id)
			return
		}
	}
	if raw, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.EscapedPath(), "/_localbase/models/"), "/admission"); ok && raw != "" {
		if id, err := url.PathUnescape(raw); err == nil {
			h.opts.AdmissionAPI(w, r, id)
			return
		}
	}
	if raw, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.EscapedPath(), "/_localbase/models/"), "/unload"); ok && raw != "" {
		if id, err := url.PathUnescape(raw); err == nil {
			h.unloadAPI(w, r, id)
			return
		}
	}
	if r.Method != http.MethodGet {
		// Checked here, not by the route table's gate: the method allowed depends on the path below the route.
		httpx.MethodNotAllowed(w, r, "GET")
		return
	}
	snap := h.opts.Runtimes().LLM.Snapshot()
	voices, _ := h.opts.Store.Voices(r.Context())
	in := meta.Input{Store: h.opts.Store, RootDir: h.opts.Root.Dir, Config: h.opts.Runtimes().LLM.Config(), MemoryGB: h.opts.MemoryGB, Voices: voices, Snapshot: &snap, Catalog: h.opts.Catalog}
	if r.URL.Path != "/_localbase/models" {
		raw := strings.TrimPrefix(r.URL.EscapedPath(), "/_localbase/models/")
		id, err := url.PathUnescape(raw)
		if raw == "" || err != nil {
			httpx.RouteDisabled(w, r)
			return
		}
		spec := h.opts.Catalog.ByID(id)
		if spec == nil {
			httpx.ModelNotFound(w, r, id)
			return
		}
		m, problems := meta.Project(r.Context(), in, spec)
		h.opts.ReportReads(w, problems)
		if len(problems) == 0 && (spec.Kind != "llm" || h.opts.Placement == nil) {
			httpx.JSON(w, 200, m)
			return
		}
		// Go only (DESIGN §20.2): device.placement is the split the loaded
		// model runs with, or the next load's when it is not loaded.
		b, _ := json.Marshal(m)
		var out map[string]any
		json.Unmarshal(b, &out)
		if len(problems) > 0 {
			out["degraded"], out["readProblems"] = true, httpx.ReadProblemsJSON(problems)
		}
		if dev, ok := out["device"].(map[string]any); ok && spec.Kind == "llm" && h.opts.Placement != nil {
			if plan, ok := h.opts.Runtimes().LLM.LoadedPlacement(id); ok && plan.Placement != nil {
				dev["placement"] = placementJSON(plan)
			} else if next, err := h.opts.Runtimes().LLM.PreviewPlacement(id, h.opts.Placement.Request(id)); err == nil && next.Placement != nil {
				pj := placementJSON(next)
				delete(pj, "plannedAt")
				pj["preview"] = true
				dev["placement"] = pj
			}
		}
		httpx.JSON(w, 200, out)
		return
	}
	var data []meta.Model
	var problems []*store.ReadError
	for _, spec := range h.opts.Catalog.All() {
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
	topo := h.opts.Memory.Topology()
	var avail any
	if hs := h.opts.Memory.Snapshot(r.Context()); hs.Available > 0 {
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
		h.opts.ReportReads(w, problems)
		out["degraded"], out["readProblems"], out["unknown"] = true, httpx.ReadProblemsJSON(problems), unknown
	}
	httpx.JSON(w, 200, out)
}
