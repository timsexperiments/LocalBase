package gateway

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/library"
	"github.com/timsexperiments/LocalBase/go/internal/models"
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
	Capabilities  any                    `json:"capabilities,omitempty"` // []catalog.Capability; [] is kept
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
	for _, e := range s.importedLLMs() {
		id := e.Model.ID
		if seen[id] || (only != "" && id != only) {
			continue
		}
		task := importTask(&e.Model)
		if p != nil && !p.Can(map[string]string{"chat": "inference:chat", "embeddings": "inference:embeddings", "decision": "inference:decision"}[task]) {
			continue
		}
		seen[id] = true
		m := s.project("llm", id, nil, runningID, runningPlan)
		m.Name = e.Model.Name
		switch task {
		case "embeddings":
			m.Architecture.Output = []string{"embeddings"}
		case "decision":
			m.Architecture.Output = []string{"decisions"}
		}
		if slices.Contains(e.Model.Capabilities, "vision") {
			m.Architecture.Input = []string{"text", "image"}
		}
		out = append(out, m)
	}
	return out
}

// importedLLMs lists imported models the LLM runtime serves.
func (s *Server) importedLLMs() []library.Entry {
	if s.Library == nil {
		return nil
	}
	entries, err := s.Library.Entries(context.Background())
	if err != nil {
		return nil
	}
	var out []library.Entry
	for _, e := range entries {
		if e.Imported && importTask(&e.Model) != "" {
			out = append(out, e)
		}
	}
	return out
}

func importTask(m *models.Model) string {
	for _, t := range []string{"chat", "embeddings", "decision"} {
		if m.HasTask(t) {
			return t
		}
	}
	return ""
}

// importedTask is an imported model's LLM task, or "".
func (s *Server) importedTask(id string) string {
	for _, e := range s.importedLLMs() {
		if e.Model.ID == id {
			return importTask(&e.Model)
		}
	}
	return ""
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
		caps := s.capabilities(context.Background(), spec).Capabilities
		if caps == nil {
			caps = []catalog.Capability{}
		}
		m.Capabilities = caps
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
	}
	if spec != nil {
		m.Memory = s.fit(spec)
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

// capabilities resolves a model's effective capabilities (declared, detected
// from its files, reported by its runtime).
func (s *Server) capabilities(ctx context.Context, spec *catalog.Spec) runtime.Resolved {
	return runtime.CapabilitiesFor(ctx, s.Store, spec, s.Root.ModelsDir(spec.Kind))
}

// fit is a model's memory figure on this machine.
func (s *Server) fit(spec *catalog.Spec) *runtime.Fit {
	return runtime.ModelFit(context.Background(), s.Store, spec, s.Root.ModelsDir(spec.Kind), s.MemoryGB)
}
