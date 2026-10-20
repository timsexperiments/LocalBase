package gateway

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Model is one /v1/models entry.
type Model struct {
	ID            string                 `json:"id"`
	Object        string                 `json:"object"`
	Created       int64                  `json:"created"`
	OwnedBy       string                 `json:"owned_by"`
	Name          string                 `json:"name,omitempty"`
	ContextLength *int                   `json:"context_length,omitempty"`
	ContextBudget *runtime.ContextBudget `json:"context_budget,omitempty"`
	Memory        *runtime.Fit           `json:"memory,omitempty"`
	Capabilities  []catalog.Capability   `json:"capabilities,omitempty"`
	Architecture  struct {
		Input  []string `json:"input_modalities"`
		Output []string `json:"output_modalities"`
	} `json:"architecture"`
}

var defaultModalities = map[string][2][]string{
	"llm": {{"text"}, {"text"}}, "stt": {{"audio"}, {"text"}}, "tts": {{"text"}, {"audio"}},
	"image": {{"text"}, {"image"}}, "video": {{"text"}, {"video"}},
}

func permissionFor(kind string, spec *catalog.Spec) string {
	switch kind {
	case "llm":
		switch {
		case spec != nil && spec.IsEmbedding():
			return "inference:embeddings"
		case spec != nil && spec.IsDecision():
			return "inference:decision"
		}
		return "inference:chat"
	case "stt":
		return "inference:transcription"
	case "tts":
		return "inference:speech"
	case "image":
		return "inference:image"
	}
	return "inference:video"
}

// ServedModels lists what the gateway routes to: per kind the active model
// plus selected ones, filtered to what the principal may invoke (nil = all).
func (s *Server) ServedModels(p *auth.Principal, only string) []Model {
	cfg := s.LLM.Config()
	runningID, runningPlan, _ := s.LLM.Running()
	var out []Model
	seen := map[string]bool{}
	for _, kind := range store.Kinds {
		if kind != "llm" && len(cfg.Selected[kind]) == 0 {
			continue
		}
		for _, id := range runtime.ConfiguredIDs(cfg, kind) {
			if seen[id] || (only != "" && id != only) {
				continue
			}
			spec := s.Catalog.ByID(id)
			if p != nil && !p.Can(permissionFor(kind, spec)) {
				continue
			}
			seen[id] = true
			out = append(out, s.project(kind, id, spec, runningID, runningPlan))
		}
	}
	return out
}

func (s *Server) project(kind, id string, spec *catalog.Spec, runningID string, runningPlan *runtime.LLMPlan) Model {
	m := Model{ID: id, Object: "model", Created: 1670000000, OwnedBy: "local-base"}
	mod := defaultModalities[kind]
	m.Architecture.Input, m.Architecture.Output = mod[0], mod[1]
	if spec != nil {
		m.Name = spec.Family
		m.Architecture.Input, m.Architecture.Output = spec.InputModalities, spec.OutputModalities
		switch {
		case spec.IsEmbedding():
			m.Architecture.Output = []string{"embeddings"}
		case spec.IsDecision():
			m.Architecture.Output = []string{"decisions"}
		}
		for _, c := range spec.Capabilities {
			m.Capabilities = append(m.Capabilities, catalog.Capability{ID: c.ID, Source: "declared"})
		}
	}
	if kind == "llm" {
		plan := runningPlan
		if id != runningID {
			plan, _ = s.LLM.Plan(id)
		}
		if plan != nil {
			ctx := plan.Budget.EffectiveContext
			m.ContextLength = &ctx
			b := plan.Budget
			m.ContextBudget = &b
		}
		if spec != nil {
			var peaks []store.ObservedPeak
			if sha := spec.Primary().SHA256; sha != "" {
				peaks, _ = s.Store.ObservedPeaks(context.Background(), spec.ModelID, sha)
			}
			var geo *memory.Geometry
			if plan != nil {
				geo = plan.Geometry
			}
			m.Memory = runtime.FitFor(spec, s.MemoryGB, geo, peaks)
		}
	}
	return m
}

func (s *Server) principalForListing(r *http.Request) *auth.Principal {
	if s.NoAuth {
		return nil
	}
	return PrincipalFrom(r.Context())
}

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r, "GET")
		return
	}
	data := s.ServedModels(s.principalForListing(r), "")
	if data == nil {
		data = []Model{}
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

// retrieveModel serves GET /v1/models/{model}: percent-decoded, the rest of
// the path is the id, and ids the key cannot list are 404 (#362).
func (s *Server) retrieveModel(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimPrefix(r.URL.EscapedPath(), "/v1/models/")
	id, err := url.PathUnescape(raw)
	if raw == "" || err != nil {
		routeDisabled(w, r)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r, "GET")
		return
	}
	data := s.ServedModels(s.principalForListing(r), id)
	if len(data) == 0 {
		modelNotFound(w, r, id)
		return
	}
	writeJSON(w, 200, data[0])
}
