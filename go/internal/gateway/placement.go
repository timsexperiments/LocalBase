package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/placement"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// placementBody is a placement change (DESIGN §20.2). contextSize is the
// shared ctxSize setting, saved with ctxMode "explicit" (§20.8 item 2).
type placementBody struct {
	Mode         string `json:"mode"`
	CPUMoELayers *int   `json:"cpuMoeLayers"`
	GPULayers    *int   `json:"gpuLayers"`
	ContextSize  *int   `json:"contextSize"`
}

// placementAPI serves /_localbase/models/{id}/placement: GET (models:read)
// shows the stored choice, the split the loaded model runs with, and what
// the next load would do; PUT and DELETE (models:manage) change the stored
// choice, which applies on the next load. The runtime is never restarted
// here, and a change is refused while the model is loading.
func (s *Server) placementAPI(w http.ResponseWriter, r *http.Request, id string) {
	w.Header().Set("Cache-Control", "no-store")
	if s.Placement == nil || s.LLM == nil {
		routeDisabled(w, r)
		return
	}
	if _, _, err := s.LLM.Resolve(id); err != nil {
		modelNotFound(w, r, id)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.placementState(w, r, id, nil)
		return
	case http.MethodPut, http.MethodDelete:
	default:
		methodNotAllowed(w, r, "GET, PUT, DELETE")
		return
	}
	if p := s.principal(r); !s.NoAuth && !p.Can("models:manage") {
		forbidden(w, r)
		return
	}
	if s.LLM.Loading(id) {
		writeError(w, r, 409, "invalid_request_error", "model_loading", "The model is loading. Change its placement once it has loaded; the change then applies on the next load.", nil)
		return
	}
	ctx := r.Context()
	if r.Method == http.MethodDelete {
		if _, err := s.Store.ResetPlacement(ctx, id, s.Placement.DeviceID); err != nil {
			serviceUnavailable(w, r, "Model management")
			return
		}
		s.placementState(w, r, id, nil)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4097))
	var in placementBody
	if err != nil || len(raw) > 4096 || json.Unmarshal(raw, &in) != nil {
		badRequest(w, r, `Send {"mode": "auto"|"gpu"|"cpu"|"custom", "cpuMoeLayers"?, "gpuLayers"?, "contextSize"?}.`, nil)
		return
	}
	if in.Mode == "" {
		in.Mode = placement.Auto
	}
	if in.Mode != placement.Custom && (in.CPUMoELayers != nil || in.GPULayers != nil) {
		writeError(w, r, 400, "invalid_request_error", "placement_unsupported", "CPU expert layers and GPU layers apply to custom placement only.", nil)
		return
	}
	if in.ContextSize != nil && (*in.ContextSize < 256 || *in.ContextSize > 2_147_483_647) {
		writeError(w, r, 400, "invalid_request_error", "placement_unsupported", "The context size must be at least 256 tokens.", strp("contextSize"))
		return
	}
	req := placement.Request{Mode: in.Mode, CPUMoELayers: in.CPUMoELayers, GPULayers: in.GPULayers}
	// Validate against this device before storing: unsupported is 400,
	// cannot fit is 409, with the numbers.
	if _, err := s.LLM.PreviewPlacement(id, req); err != nil {
		var pe *placement.Error
		if errors.As(err, &pe) {
			status := 400
			if pe.Code == "placement_does_not_fit" {
				status = 409
			}
			writeError(w, r, status, "invalid_request_error", pe.Code, pe.Message, nil)
			return
		}
	}
	if in.ContextSize != nil {
		if err := s.updateConfig(func(c *store.Config) error {
			c.CtxSize, c.CtxMode = *in.ContextSize, "explicit"
			return nil
		}); err != nil {
			serviceUnavailable(w, r, "Model management")
			return
		}
	}
	if err := s.Store.SetPlacement(ctx, id, s.Placement.DeviceID, store.Placement{Mode: in.Mode, GPULayers: in.GPULayers, CPUMoELayers: in.CPUMoELayers}); err != nil {
		serviceUnavailable(w, r, "Model management")
		return
	}
	s.Log.Info("Placement changed for "+id+"; it applies on the next load.", "event", "models.placement-changed", "model_id", id, "mode", in.Mode)
	s.placementState(w, r, id, nil)
}

// placementState answers the stored choice, the loaded split and the next
// load's split.
func (s *Server) placementState(w http.ResponseWriter, r *http.Request, id string, _ error) {
	req := s.Placement.Request(id)
	cfg := s.LLM.Config()
	requested := map[string]any{"mode": req.Mode, "cpuMoeLayers": req.CPUMoELayers, "gpuLayers": req.GPULayers,
		"contextSize": cfg.CtxSize, "contextMode": cfg.CtxMode, "contextScope": "all models"}
	out := map[string]any{"model": id, "requested": requested, "loaded": false, "resolved": nil, "pendingNextLoad": false}
	if plan, ok := s.LLM.LoadedPlacement(id); ok && plan.Placement != nil {
		out["loaded"] = true
		out["resolved"] = placementJSON(plan)
		out["pendingNextLoad"] = !sameRequest(plan.PlacementRequest, req) || plan.Budget.ConfiguredCeiling != cfg.CtxSize
	}
	if next, err := s.LLM.PreviewPlacement(id, req); err == nil && next.Placement != nil {
		out["next"] = placementJSON(next)
	} else if err != nil {
		var pe *placement.Error
		if errors.As(err, &pe) {
			out["nextError"] = map[string]any{"code": pe.Code, "message": pe.Message}
		}
	}
	writeJSON(w, 200, out)
}

func placementJSON(plan *runtime.LLMPlan) map[string]any {
	p := plan.Placement
	b, _ := json.Marshal(p)
	var m map[string]any
	json.Unmarshal(b, &m)
	m["plannedAt"] = plan.PlacedAt.Format(time.RFC3339)
	m["contextSize"] = plan.CtxSize
	return m
}

func sameRequest(a, b placement.Request) bool {
	eq := func(x, y *int) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	ma, mb := a.Mode, b.Mode
	if ma == "" {
		ma = placement.Auto
	}
	if mb == "" {
		mb = placement.Auto
	}
	return ma == mb && eq(a.CPUMoELayers, b.CPUMoELayers) && eq(a.GPULayers, b.GPULayers)
}

// placementRuntimeState tells the local CLI, behind the instance token,
// whether a model is loading and the split it was loaded with.
func (s *Server) placementRuntimeState(w http.ResponseWriter, r *http.Request) {
	o := s.Root.Instance()
	if o == nil || r.Header.Get("X-Localbase-Instance-Token") != o.InstanceToken || s.LLM == nil {
		w.WriteHeader(404)
		return
	}
	id := r.URL.Query().Get("model")
	out := map[string]any{"loading": s.LLM.Loading(id), "loaded": false}
	if plan, ok := s.LLM.LoadedPlacement(id); ok && plan.Placement != nil {
		out["loaded"], out["resolved"] = true, placementJSON(plan)
		req := plan.PlacementRequest
		if req.Mode == "" {
			req.Mode = placement.Auto
		}
		out["loadedRequest"] = req
	}
	writeJSON(w, 200, out)
}
