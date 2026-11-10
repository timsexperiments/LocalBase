package infer

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
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
func (h *Handler) ServedModels(p *auth.Principal, only string) ([]Model, []*store.ReadError) {
	cfg := h.opts.Runtimes().LLM.Config()
	runningID, runningPlan, _ := h.opts.Runtimes().LLM.Running()
	var out []Model
	var problems []*store.ReadError
	seen := map[string]bool{}
	for _, kind := range store.Kinds {
		if kind != "llm" && len(cfg.Selected[kind]) == 0 {
			continue
		}
		for _, id := range runtime.ConfiguredIDs(cfg, kind) {
			if seen[id] || (only != "" && id != only) {
				continue
			}
			spec := h.opts.Catalog.ByID(id)
			if p != nil && !p.Can(permissionFor(kind, spec)) {
				continue
			}
			seen[id] = true
			m, failed := h.project(kind, id, spec, runningID, runningPlan)
			problems = append(problems, failed...)
			out = append(out, m)
		}
	}
	imported, err := h.importedLLMsChecked()
	if err != nil {
		problems = append(problems, &store.ReadError{Step: "reading the imported models", Err: err, Kind: store.ReadLibrary})
	}
	for _, e := range imported {
		id := e.Model.ID
		if seen[id] || (only != "" && id != only) {
			continue
		}
		task := models.ImportTask(&e.Model)
		if p != nil && !p.Can(map[string]string{"chat": "inference:chat", "embeddings": "inference:embeddings", "decision": "inference:decision"}[task]) {
			continue
		}
		seen[id] = true
		m, failed := h.project("llm", id, nil, runningID, runningPlan)
		problems = append(problems, failed...)
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
	return out, problems
}

func (h *Handler) importedLLMsChecked() ([]library.Entry, error) {
	if h.opts.Library() == nil {
		return nil, nil
	}
	entries, err := h.opts.Library().Entries(context.Background())
	if err != nil {
		return nil, err
	}
	var out []library.Entry
	for _, e := range entries {
		if e.Imported && models.ImportTask(&e.Model) != "" {
			out = append(out, e)
		}
	}
	return out, nil
}

func (h *Handler) project(kind, id string, spec *catalog.Spec, runningID string, runningPlan *runtime.LLMPlan) (Model, []*store.ReadError) {
	var problems []*store.ReadError
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
		resolved := h.capabilities(context.Background(), spec)
		if resolved.ReadProblem != nil {
			problems = append(problems, resolved.ReadProblem)
		}
		caps := resolved.Capabilities
		if caps == nil {
			caps = []catalog.Capability{}
		}
		m.Capabilities = caps
	}
	if kind == "llm" {
		plan := runningPlan
		if id != runningID {
			plan, _ = h.opts.Runtimes().LLM.Plan(id)
		}
		if plan != nil {
			ctx := plan.Budget.EffectiveContext
			m.ContextLength = &ctx
			b := plan.Budget
			m.ContextBudget = &b
		}
	}
	if spec != nil {
		// Leaving out the figure, not giving one without what was seen.
		fit, err := h.fit(spec)
		var failed *store.ReadError
		if errors.As(err, &failed) {
			problems = append(problems, failed)
		} else {
			m.Memory = fit
		}
	}
	return m, problems
}

func (h *Handler) principalForListing(r *http.Request) *auth.Principal {
	if h.opts.NoAuth() {
		return nil
	}
	return httpx.PrincipalFrom(r.Context())
}

func (h *Handler) ListModels(w http.ResponseWriter, r *http.Request) {
	// A client's list stays a list: what was missing is in a header and the log, not an outage.
	data, problems := h.ServedModels(h.principalForListing(r), "")
	h.opts.ReportReads(w, problems)
	if data == nil {
		data = []Model{}
	}
	httpx.JSON(w, 200, map[string]any{"object": "list", "data": data})
}

// retrieveModel serves GET /v1/models/{model}: percent-decoded, the rest of
// the path is the id, and ids the key cannot list are 404 (#362).
func (h *Handler) RetrieveModel(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimPrefix(r.URL.EscapedPath(), "/v1/models/")
	id, err := url.PathUnescape(raw)
	if raw == "" || err != nil {
		httpx.RouteDisabled(w, r)
		return
	}
	if r.Method != http.MethodGet {
		// Checked here, not by the route table's gate: an id that can't be read is answered 404 first.
		httpx.MethodNotAllowed(w, r, "GET")
		return
	}
	data, problems := h.ServedModels(h.principalForListing(r), id)
	h.opts.ReportReads(w, problems)
	if len(data) == 0 {
		// It might be one of the models that couldn't be read, so "not found" would be a guess.
		for _, p := range problems {
			if p.Kind == store.ReadLibrary {
				w.Header().Set("Retry-After", "5")
				httpx.Error(w, r, 503, "api_error", "service_unavailable",
					"Could not finish "+p.Step+", so this model may exist. Try again shortly.", nil)
				return
			}
		}
		httpx.ModelNotFound(w, r, id)
		return
	}
	httpx.JSON(w, 200, data[0])
}

// capabilities resolves a model's effective capabilities (declared, detected
// from its files, reported by its runtime).
func (h *Handler) capabilities(ctx context.Context, spec *catalog.Spec) runtime.Resolved {
	return runtime.CapabilitiesFor(ctx, h.opts.Store, h.opts.Catalog, spec, h.opts.Root.ModelsDir(spec.Kind))
}

// fit is a model's memory figure on this machine.
func (h *Handler) fit(spec *catalog.Spec) (*runtime.Fit, error) {
	return runtime.ModelFit(context.Background(), h.opts.Store, h.opts.Catalog, spec, h.opts.Root.ModelsDir(spec.Kind), h.opts.MemoryGB)
}
