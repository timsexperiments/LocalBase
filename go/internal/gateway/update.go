package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
	"github.com/timsexperiments/LocalBase/go/internal/usage"
)

// The optional daily check for a newer version (DESIGN §33.1). Reading says what is stored and
// asks nobody. This route never installs anything: an upgrade restarts the gateway serving it,
// and it belongs on the computer.
func (s *Server) updateAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		p := s.principal(r)
		if !p.Authenticated() {
			httpx.Unauthorized(w, r)
			return
		}
		if !s.NoAuth && !p.Can("models:read") {
			httpx.Forbidden(w, r)
			return
		}
		if s.Updates == nil {
			notConfigured(w, r, "Update check")
			return
		}
		httpx.JSON(w, http.StatusOK, s.Updates.Read(r.Context()))
	case http.MethodPost:
		s.updateChange(w, r)
	default:
		// Checked here, not by the route table's gate: the 405 carries this handler's Cache-Control.
		httpx.MethodNotAllowed(w, r, "GET, POST")
	}
}

func (s *Server) updateChange(w http.ResponseWriter, r *http.Request) {
	actorKind, actorID := "cli", ""
	if r.Header.Get("X-Localbase-Instance-Token") != "" {
		if !s.localCLI(r) {
			httpx.Unauthorized(w, r)
			return
		}
	} else {
		p := s.principal(r)
		if !p.Authenticated() {
			httpx.Unauthorized(w, r)
			return
		}
		if !s.NoAuth && !usage.CanChange(p.Can) {
			httpx.Forbidden(w, r)
			return
		}
		actorKind, actorID, _ = s.usagePrincipal(p)
	}
	if s.Updates == nil {
		notConfigured(w, r, "Update check")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
	if err != nil {
		httpx.BadRequest(w, r, "Invalid update request.", nil)
		return
	}
	if len(body) > 4096 {
		httpx.PayloadTooLarge(w, r)
		return
	}
	var in struct {
		Action  string `json:"action"`
		Enabled *bool  `json:"enabled"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || in.Action != "set-check" || in.Enabled == nil {
		httpx.BadRequest(w, r, `Expected {"action":"set-check","enabled":true or false}.`, nil)
		return
	}
	if err := s.Updates.SetEnabled(r.Context(), *in.Enabled); err != nil {
		s.Log.Error("Could not change the update check.", "event", "update.change-failed", "error", err.Error())
		httpx.ServiceUnavailable(w, r, "Update check")
		return
	}
	s.Log.Info("Administration change.", "event", "audit.update-changed", "action", in.Action, "enabled", *in.Enabled, "actor_kind", actorKind, "actor_id", actorID)
	httpx.JSON(w, http.StatusOK, s.Updates.Read(r.Context()))
}
