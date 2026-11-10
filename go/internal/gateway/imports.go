package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/timsexperiments/LocalBase/go/internal/acquire"
	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/backends"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
	"github.com/timsexperiments/LocalBase/go/internal/lease"
	"github.com/timsexperiments/LocalBase/go/internal/library"
	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
)

// Admin APIs for model import and backend selection (DESIGN §12.6):
//
//	GET  /_localbase/backends                 models:read
//	GET  /_localbase/model-backend?model=ID   models:read
//	POST /_localbase/model-backend            models:manage  {action: set|reset, model, backendId?, variantId?}
//	POST /_localbase/model-imports            models:manage  {action: resolve|start, source, ...}
//	GET  /_localbase/model-imports?id=JOB     models:manage
//
// Responses never contain header values, tokens or signed URL queries.

func (h *Handler) adminPrincipal(w http.ResponseWriter, r *http.Request, permission string) *auth.Principal {
	p := h.opts.Caller(r)
	if !p.Authenticated() {
		httpx.Unauthorized(w, r)
		return nil
	}
	if !p.Can(permission) {
		httpx.Forbidden(w, r)
		return nil
	}
	return p
}

func (h *Handler) libraryOr503(w http.ResponseWriter, r *http.Request) *library.Library {
	if h.opts.Library() == nil {
		httpx.ServiceUnavailable(w, r, "Model management")
	}
	return h.opts.Library()
}

func (h *Handler) BackendsAPI(w http.ResponseWriter, r *http.Request) {
	if h.adminPrincipal(w, r, "models:read") == nil {
		return
	}
	l := h.libraryOr503(w, r)
	if l == nil {
		return
	}
	out := []map[string]any{}
	for _, d := range l.Registry.All() {
		out = append(out, map[string]any{"id": d.ID, "name": d.Name, "engine": d.Engine, "builtIn": d.BuiltIn,
			"adapter": d.Adapter, "tasks": d.Tasks, "install": l.Host().Install[d.ID], "installLabel": l.Host().InstallLabel(d.ID),
			"automatic": d.Automatic == nil || *d.Automatic, "automaticTasks": d.AutomaticTasks, "notes": d.Notes})
	}
	httpx.NoStore(w, 200, map[string]any{"backends": out, "skipped": l.Registry.Skipped(), "host": l.Host()})
}

func (h *Handler) ModelBackendAPI(w http.ResponseWriter, r *http.Request) {
	perm := "models:read"
	if r.Method == http.MethodPost {
		perm = "models:manage"
	}
	if h.adminPrincipal(w, r, perm) == nil {
		return
	}
	l := h.libraryOr503(w, r)
	if l == nil {
		return
	}
	ctx := r.Context()
	reply := func(id string, sel backends.Selection, err error) {
		switch {
		case errors.Is(err, library.ErrNotFound):
			httpx.ModelNotFound(w, r, id)
		case err != nil:
			httpx.BadRequest(w, r, err.Error(), nil)
		default:
			httpx.NoStore(w, 200, map[string]any{"model": id, "selection": sel})
		}
	}
	if r.Method == http.MethodGet {
		id := r.URL.Query().Get("model")
		e, err := l.Get(ctx, id)
		if err != nil {
			reply(id, backends.Selection{}, err)
			return
		}
		sel, err := l.Selection(ctx, e)
		reply(id, sel, err)
		return
	}
	var in struct {
		Action    string `json:"action"`
		Model     string `json:"model"`
		BackendID string `json:"backendId"`
		VariantID string `json:"variantId"`
	}
	body, ok := httpx.ReadManagement(w, r)
	if !ok {
		return
	}
	if !httpx.StrictDecode(body, &in) || (in.Action != "set" && in.Action != "reset") || in.Model == "" || (in.Action == "set" && in.BackendID == "") {
		httpx.BadRequest(w, r, "Expected {action: set|reset, model, backendId (for set), variantId?}.", nil)
		return
	}
	var sel backends.Selection
	err := lease.WithOperation(h.opts.Root.Dir, "update runtime configuration", func() (err error) {
		if in.Action == "set" {
			sel, err = l.SetBackend(ctx, in.Model, in.BackendID, in.VariantID)
		} else {
			sel, err = l.ResetBackend(ctx, in.Model)
		}
		return err
	})
	reply(in.Model, sel, err)
}

func (h *Handler) ModelImportsAPI(w http.ResponseWriter, r *http.Request) {
	if h.adminPrincipal(w, r, "models:manage") == nil {
		return
	}
	l := h.libraryOr503(w, r)
	if l == nil {
		return
	}
	if r.Method == http.MethodGet {
		job := l.Job(r.URL.Query().Get("id"))
		if job == nil {
			httpx.Error(w, r, 404, "invalid_request_error", "import_not_found", "No import with that id.", nil)
			return
		}
		b, _ := json.Marshal(job)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
		return
	}
	body, ok := httpx.ReadManagement(w, r)
	if !ok {
		return
	}
	var in struct {
		Action      string   `json:"action"` // resolve | start | cancel
		Job         string   `json:"job"`    // cancel
		Source      string   `json:"source"`
		Files       []string `json:"files"`
		Revision    string   `json:"revision"`
		SHA256      string   `json:"sha256"`
		AuthProfile string   `json:"authProfile"`
		Task        string   `json:"task"`
		Backend     string   `json:"backend"`
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		AllowHTTP   bool     `json:"allowHttp"`
		Confirm     bool     `json:"confirmCustomSource"`
	}
	if !httpx.StrictDecode(body, &in) {
		httpx.BadRequest(w, r, "Expected {action: resolve|start, source, ...} or {action: cancel, job}.", nil)
		return
	}
	if in.Action == "cancel" {
		if in.Job == "" || !httpx.OnlyFields(body, "job") {
			httpx.BadRequest(w, r, "Expected {action: cancel, job}.", httpx.StrPtr("job"))
			return
		}
		if !l.CancelImport(in.Job) {
			httpx.Error(w, r, 404, "invalid_request_error", "import_not_found", "No import with that id.", httpx.StrPtr("job"))
			return
		}
		httpx.NoStore(w, 200, l.Job(in.Job))
		return
	}
	if (in.Action != "resolve" && in.Action != "start") || in.Source == "" {
		httpx.BadRequest(w, r, "Expected {action: resolve|start, source, ...} or {action: cancel, job}.", nil)
		return
	}
	if in.Task != "" && !slices.Contains(models.Tasks, in.Task) {
		httpx.BadRequest(w, r, "Unknown task.", nil)
		return
	}
	if in.Backend != "" && in.Backend != "auto" && l.Registry.Get(in.Backend) == nil {
		httpx.BadRequest(w, r, "That backend is not registered.", nil)
		return
	}
	cfg, _ := h.opts.Store.LoadConfig(r.Context())
	token := ""
	if cfg != nil {
		token = cfg.HFToken
	}
	plan, err := acquire.Resolve(r.Context(), h.opts.Root.Dir, acquire.Request{Source: in.Source, Files: in.Files, Revision: in.Revision,
		SHA256: in.SHA256, AuthProfile: in.AuthProfile, AllowHTTP: in.AllowHTTP, ID: in.ID, Name: in.Name, Task: in.Task, HFToken: token,
		AllowPrivate: h.opts.Store.PrivateImportsAllowed(r.Context()), AllowLocal: h.opts.Store.LocalPathImportsAllowed(r.Context()), Confirmed: in.Confirm, Log: h.opts.Log()})
	var refused *acquire.RefusedError
	if errors.As(err, &refused) {
		httpx.Error(w, r, http.StatusForbidden, "invalid_request_error", refused.Code, refused.Message, httpx.StrPtr("source"))
		return
	}
	if err != nil {
		httpx.Error(w, r, 400, "invalid_request_error", "import_unresolved", obs.BoundedText(err.Error(), 1024), nil)
		return
	}
	if in.Action == "resolve" {
		httpx.NoStore(w, 200, map[string]any{"plan": plan, "recommendation": l.Preview(plan)})
		return
	}
	switch {
	case len(plan.Choices) > 0:
		httpx.Error(w, r, 400, "invalid_request_error", "import_choose_file", "The repository has several weight files; choose one.", nil)
		return
	case plan.Custom && !in.Confirm:
		httpx.Error(w, r, 400, "invalid_request_error", "custom_source_unconfirmed", acquire.CustomSourceWarning, nil)
		return
	case plan.Task == "":
		httpx.Error(w, r, 400, "invalid_request_error", "import_task_required", "Choose what the model does.", nil)
		return
	}
	if err := acquire.CheckSpace(h.opts.Root.Dir, plan); errors.As(err, &refused) {
		httpx.Error(w, r, http.StatusInsufficientStorage, "invalid_request_error", refused.Code, refused.Message, nil)
		return
	}
	id, err := l.StartImport(plan, in.Backend, h.opts.Log())
	switch {
	case errors.Is(err, library.ErrImportsBusy):
		w.Header().Set("Retry-After", "5")
		httpx.Error(w, r, 429, "rate_limit_error", "import_busy", err.Error(), nil)
		return
	case errors.Is(err, library.ErrImportRunning):
		httpx.Error(w, r, 409, "invalid_request_error", "import_running", err.Error(), nil)
		return
	case err != nil:
		httpx.ServiceUnavailable(w, r, "Model management")
		return
	}
	if in.Backend != "" && in.Backend != "auto" {
		kind, actor := h.opts.AuditActor(r)
		h.opts.Log().Info("Administration change.", "event", "audit.model-backend-changed", "action", "import", "actor_kind", kind, "actor_id", actor,
			"target_kind", "model", "target_id", plan.ID, "backend_id", in.Backend)
	}
	httpx.NoStore(w, 202, map[string]any{"job": id, "plan": plan})
}

// authProfilesAPI lists auth profiles: ids, origins and header names. Header
// values never leave the credential file.
func (h *Handler) AuthProfilesAPI(w http.ResponseWriter, r *http.Request) {
	if h.adminPrincipal(w, r, "models:manage") == nil {
		return
	}
	ps, err := acquire.LoadProfiles(h.opts.Root.Dir)
	if err != nil {
		httpx.ServiceUnavailable(w, r, "Model management")
		return
	}
	out := []acquire.PublicProfile{}
	for _, p := range ps.Profiles {
		out = append(out, p.Public())
	}
	httpx.NoStore(w, 200, map[string]any{"profiles": out})
}
