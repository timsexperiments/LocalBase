package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// defaultChatModelsAPI reads and sets the ordered default chat model list
// (Go only, DESIGN §18). GET needs models:read; POST {"models": [...]}
// needs models:manage. One id makes it the active LLM alone; the list
// holds selected chat models.
func (s *Server) defaultChatModelsAPI(w http.ResponseWriter, r *http.Request) {
	p := s.principal(r)
	if !p.Authenticated() {
		unauthorized(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	reply := func() {
		c := s.LLM.DefaultModel(false)
		writeJSON(w, 200, map[string]any{"models": s.LLM.Config().DefaultLLMList(),
			"effective": map[string]any{"modelId": c.ModelID, "preferredModelId": c.Preferred, "reason": c.Reason}})
	}
	switch r.Method {
	case http.MethodGet:
		if !p.Can("models:read") {
			forbidden(w, r)
			return
		}
		reply()
		return
	case http.MethodPost:
	default:
		methodNotAllowed(w, r, "GET, POST")
		return
	}
	if !p.Can("models:manage") {
		forbidden(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
	var in struct {
		Models []string `json:"models"`
	}
	if err != nil || len(body) > 4096 || json.Unmarshal(body, &in) != nil || len(in.Models) == 0 || len(in.Models) > 8 {
		badRequest(w, r, "Send {\"models\": [...]} with one to eight chat model IDs, first choice first.", nil)
		return
	}
	err = s.updateConfig(func(c *store.Config) error {
		seen := map[string]bool{}
		for _, id := range in.Models {
			spec := s.Catalog.ByID(id)
			switch {
			case !slices.Contains(c.Selected["llm"], id):
				return &mgmtError{"invalid_request", id + " is not an enabled chat model."}
			case spec != nil && (spec.IsDecision() || spec.IsEmbedding()):
				return &mgmtError{"invalid_request", id + " is not a chat model."}
			case seen[id]:
				return &mgmtError{"invalid_request", "Each model can appear once."}
			}
			seen[id] = true
		}
		// One model is the active LLM itself. A list leaves activeLlmModel
		// alone: TS launches it at start after a rollback, without the fit
		// fallback.
		c.DefaultLLMs = nil
		if len(in.Models) == 1 {
			c.Active["llm"] = in.Models[0]
		} else {
			c.DefaultLLMs = slices.Clone(in.Models)
		}
		return nil
	})
	var me *mgmtError
	switch {
	case errors.As(err, &me):
		writeError(w, r, 400, "invalid_request_error", me.code, me.message, nil)
	case err != nil:
		s.Log.Error("Default chat models update failed.", "event", "models.default-update-failed", "error", err.Error())
		serviceUnavailable(w, r, "Model management")
	default:
		s.Log.Info("Default chat models changed.", "event", "models.defaults-changed", "models", in.Models)
		reply()
	}
}
