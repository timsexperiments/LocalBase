package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/timsexperiments/LocalBase/go/internal/acquire"
	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/backends"
	"github.com/timsexperiments/LocalBase/go/internal/library"
	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	rootpkg "github.com/timsexperiments/LocalBase/go/internal/root"
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

func (s *Server) adminPrincipal(w http.ResponseWriter, r *http.Request, permission string) *auth.Principal {
	p := s.principal(r)
	if !p.Authenticated() {
		unauthorized(w, r)
		return nil
	}
	if !p.Can(permission) {
		forbidden(w, r)
		return nil
	}
	return p
}

func (s *Server) libraryOr503(w http.ResponseWriter, r *http.Request) *library.Library {
	if s.Library == nil {
		serviceUnavailable(w, r, "Model management")
	}
	return s.Library
}

func (s *Server) backendsAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r, "GET")
		return
	}
	if s.adminPrincipal(w, r, "models:read") == nil {
		return
	}
	l := s.libraryOr503(w, r)
	if l == nil {
		return
	}
	out := []map[string]any{}
	for _, d := range l.Registry.All() {
		out = append(out, map[string]any{"id": d.ID, "name": d.Name, "engine": d.Engine, "builtIn": d.BuiltIn,
			"adapter": d.Adapter, "tasks": d.Tasks, "install": l.Host().Install[d.ID], "installLabel": l.Host().InstallLabel(d.ID),
			"automatic": d.Automatic == nil || *d.Automatic, "notes": d.Notes})
	}
	noStore(w, 200, map[string]any{"backends": out, "skipped": l.Registry.Skipped(), "host": l.Host()})
}

func (s *Server) modelBackendAPI(w http.ResponseWriter, r *http.Request) {
	perm := "models:read"
	if r.Method == http.MethodPost {
		perm = "models:manage"
	} else if r.Method != http.MethodGet {
		methodNotAllowed(w, r, "GET, POST")
		return
	}
	if s.adminPrincipal(w, r, perm) == nil {
		return
	}
	l := s.libraryOr503(w, r)
	if l == nil {
		return
	}
	ctx := r.Context()
	reply := func(id string, sel backends.Selection, err error) {
		switch {
		case errors.Is(err, library.ErrNotFound):
			modelNotFound(w, r, id)
		case err != nil:
			badRequest(w, r, err.Error(), nil)
		default:
			noStore(w, 200, map[string]any{"model": id, "selection": sel})
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
	body, ok := readManagement(w, r)
	if !ok {
		return
	}
	if !strictDecode(body, &in) || (in.Action != "set" && in.Action != "reset") || in.Model == "" || (in.Action == "set" && in.BackendID == "") {
		badRequest(w, r, "Expected {action: set|reset, model, backendId (for set), variantId?}.", nil)
		return
	}
	var sel backends.Selection
	err := rootpkg.WithOperation(s.Root.Dir, "update runtime configuration", func() (err error) {
		if in.Action == "set" {
			sel, err = l.SetBackend(ctx, in.Model, in.BackendID, in.VariantID)
		} else {
			sel, err = l.ResetBackend(ctx, in.Model)
		}
		return err
	})
	reply(in.Model, sel, err)
}

func (s *Server) modelImportsAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		methodNotAllowed(w, r, "GET, POST")
		return
	}
	if s.adminPrincipal(w, r, "models:manage") == nil {
		return
	}
	l := s.libraryOr503(w, r)
	if l == nil {
		return
	}
	if r.Method == http.MethodGet {
		job := l.Job(r.URL.Query().Get("id"))
		if job == nil {
			writeError(w, r, 404, "invalid_request_error", "import_not_found", "No import with that id.", nil)
			return
		}
		b, _ := json.Marshal(job)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
		return
	}
	body, ok := readManagement(w, r)
	if !ok {
		return
	}
	var in struct {
		Action      string   `json:"action"` // resolve | start
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
	if !strictDecode(body, &in) || (in.Action != "resolve" && in.Action != "start") || in.Source == "" {
		badRequest(w, r, "Expected {action: resolve|start, source, ...}.", nil)
		return
	}
	if in.Task != "" && !slices.Contains(models.Tasks, in.Task) {
		badRequest(w, r, "Unknown task.", nil)
		return
	}
	if in.Backend != "" && in.Backend != "auto" && l.Registry.Get(in.Backend) == nil {
		badRequest(w, r, "That backend is not registered.", nil)
		return
	}
	cfg, _ := s.Store.LoadConfig(r.Context())
	token := ""
	if cfg != nil {
		token = cfg.HFToken
	}
	plan, err := acquire.Resolve(r.Context(), s.Root.Dir, acquire.Request{Source: in.Source, Files: in.Files, Revision: in.Revision,
		SHA256: in.SHA256, AuthProfile: in.AuthProfile, AllowHTTP: in.AllowHTTP, ID: in.ID, Name: in.Name, Task: in.Task, HFToken: token})
	if err != nil {
		writeError(w, r, 400, "invalid_request_error", "import_unresolved", obs.BoundedText(err.Error(), 1024), nil)
		return
	}
	if in.Action == "resolve" {
		noStore(w, 200, map[string]any{"plan": plan, "recommendation": l.Preview(plan)})
		return
	}
	switch {
	case len(plan.Choices) > 0:
		writeError(w, r, 400, "invalid_request_error", "import_choose_file", "The repository has several weight files; choose one.", nil)
		return
	case plan.Custom && !in.Confirm:
		writeError(w, r, 400, "invalid_request_error", "custom_source_unconfirmed", acquire.CustomSourceWarning, nil)
		return
	case plan.Task == "":
		writeError(w, r, 400, "invalid_request_error", "import_task_required", "Choose what the model does.", nil)
		return
	}
	id := l.StartImport(plan, in.Backend, s.Log)
	noStore(w, 202, map[string]any{"job": id, "plan": plan})
}

// authProfilesAPI lists auth profiles: ids, origins and header names. Header
// values never leave the credential file.
func (s *Server) authProfilesAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r, "GET")
		return
	}
	if s.adminPrincipal(w, r, "models:manage") == nil {
		return
	}
	ps, err := acquire.LoadProfiles(s.Root.Dir)
	if err != nil {
		serviceUnavailable(w, r, "Model management")
		return
	}
	out := []acquire.PublicProfile{}
	for _, p := range ps.Profiles {
		out = append(out, p.Public())
	}
	noStore(w, 200, map[string]any{"profiles": out})
}
