package models

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
	"github.com/timsexperiments/LocalBase/go/internal/hub"
)

// POST /_localbase/model-discovery (models:manage): the admin UI's Hugging
// Face search, repository preview, add, and install of a published recipe
// (TS runDiscoveryRequest). Hub failures are 502s, bad input 400s; the
// token never appears in a response.

type discoveryRequest struct {
	Action   string
	Query    string
	Kind     string
	Repo     string
	Selector string
	ModelID  string
	Install  bool
}

// parseDiscovery applies the TS discriminated union (strict objects).
func parseDiscovery(b []byte) (*discoveryRequest, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var raw map[string]any
	if dec.Decode(&raw) != nil || raw == nil {
		return nil, false
	}
	str := func(k string, min, max int, required bool) (string, bool) {
		v, present := raw[k]
		if !present {
			return "", !required
		}
		s, ok := v.(string)
		n := httpx.UTF16Length(s)
		return s, ok && n >= min && n <= max
	}
	allowed := func(keys ...string) bool {
		for k := range raw {
			if !slices.Contains(keys, k) {
				return false
			}
		}
		return true
	}
	r := &discoveryRequest{}
	r.Action, _ = raw["action"].(string)
	var ok bool
	switch r.Action {
	case "search":
		if !allowed("action", "query", "kind") {
			return nil, false
		}
		if r.Query, ok = str("query", 0, 200, false); !ok {
			return nil, false
		}
		r.Kind = "llm"
		if k, present := raw["kind"]; present {
			s, isStr := k.(string)
			if !isStr || !slices.Contains(hub.AddableKinds, s) {
				return nil, false
			}
			r.Kind = s
		}
	case "preview":
		if !allowed("action", "repo") {
			return nil, false
		}
		if r.Repo, ok = str("repo", 3, 200, true); !ok {
			return nil, false
		}
	case "add":
		if !allowed("action", "repo", "selector", "modelId", "install") {
			return nil, false
		}
		var ok1, ok2, ok3 bool
		r.Repo, ok1 = str("repo", 3, 200, true)
		r.Selector, ok2 = str("selector", 1, 300, false)
		r.ModelID, ok3 = str("modelId", 1, 200, false)
		if !ok1 || !ok2 || !ok3 {
			return nil, false
		}
		if v, present := raw["install"]; present {
			if r.Install, ok = v.(bool); !ok {
				return nil, false
			}
		}
	case "install-recipe":
		if !allowed("action", "modelId") {
			return nil, false
		}
		if r.ModelID, ok = str("modelId", 1, 200, true); !ok {
			return nil, false
		}
	default:
		return nil, false
	}
	return r, true
}

var upstreamHubRe = regexp.MustCompile(`Could not reach|answered \d|not JSON|does not understand`)

func discoveryFailure(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	httpx.Error(w, r, status, "invalid_request_error", code, message, nil)
}

func (h *Handler) ModelDiscoveryAPI(w http.ResponseWriter, r *http.Request) {
	p := h.opts.Caller(r)
	if !p.Authenticated() {
		httpx.Unauthorized(w, r)
		return
	}
	if r.Method != http.MethodPost {
		// Checked here, not by the route table's gate: this handler authenticates the caller first, and 401 comes before 405.
		httpx.MethodNotAllowed(w, r, "POST")
		return
	}
	if !p.Can("models:manage") {
		httpx.Forbidden(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	body, err := io.ReadAll(io.LimitReader(r.Body, 2049))
	if r.ContentLength > 2048 || len(body) > 2048 {
		w.Header().Set("Connection", "close")
		httpx.Error(w, r, 413, "invalid_request_error", "validation_failed", "Model discovery body exceeds the 2048 byte limit.", nil)
		return
	}
	if err != nil || !json.Valid(body) {
		httpx.BadRequest(w, r, "Invalid model discovery payload.", nil)
		return
	}
	req, ok := parseDiscovery(body)
	if !ok {
		httpx.BadRequest(w, r, "Invalid model discovery payload.", nil)
		return
	}
	cfg := h.opts.Runtimes.LLM.Config()
	token := cfg.HFToken
	client := &hub.Client{Root: h.opts.Root.Dir, Token: hubTokenOr(token), Log: h.opts.Log}
	ctx := r.Context()
	fail := func(err error) {
		var ce *hub.ConflictError
		var he *hub.Error
		switch {
		case errors.Is(err, hub.ErrInvalidRepo):
			discoveryFailure(w, r, 400, "validation_failed", "Expected a Hugging Face repository as <org>/<repo>.")
		case errors.As(err, &ce):
			discoveryFailure(w, r, 409, "catalog_conflict", ce.Message)
		case errors.As(err, &he):
			upstream := upstreamHubRe.MatchString(he.Message)
			status, code := 400, "hub_rejected"
			if upstream {
				status, code = 502, "hub_unavailable"
			}
			if he.Code != "" {
				code = he.Code
			}
			discoveryFailure(w, r, status, code, he.Message)
		default:
			h.opts.Log.Error("Model discovery failed.", "event", "models.discovery-failed", "error", err.Error())
			httpx.ServiceUnavailable(w, r, "Model discovery")
		}
	}
	switch req.Action {
	case "search":
		hits, cached, err := hub.Discover(ctx, client, h.opts.Catalog, req.Kind, req.Query, h.opts.MemoryGB, 20, 12)
		if err != nil {
			fail(err)
			return
		}
		httpx.JSON(w, 200, map[string]any{"query": req.Query, "kind": req.Kind, "cached": cached, "hits": hits})
	case "preview":
		preview, err := hub.PreviewRepository(ctx, client, h.opts.Catalog, req.Repo, h.opts.MemoryGB)
		if err != nil {
			fail(err)
			return
		}
		httpx.JSON(w, 200, map[string]any{"repo": req.Repo, "url": hub.RepoURL(req.Repo), "preview": preview})
	case "install-recipe":
		model := h.opts.Catalog.ByID(req.ModelID)
		if model == nil {
			discoveryFailure(w, r, 404, "model_not_found", `"`+req.ModelID+`" is not in this catalog. Update LocalBase or the catalog feed.`)
			return
		}
		if h.opts.Catalog.RecipeAnnotation(req.ModelID) == nil {
			discoveryFailure(w, r, 400, "no_recipe", `The catalog feed has no install recipe for "`+req.ModelID+`".`)
			return
		}
		httpx.JSON(w, 200, map[string]any{"modelId": model.ModelID, "displayName": model.Name(), "kind": model.Kind, "replaced": false,
			"operation": h.discoveryInstall(model.ModelID)})
	default: // add
		added, err := hub.AddFromHub(ctx, client, h.opts.Catalog, req.Repo, req.Selector, req.ModelID)
		if err != nil {
			fail(err)
			return
		}
		// The runtimes see the new entry: the catalog is reloaded in place.
		if next, err := catalog.LoadRoot(h.opts.Root.Dir, nil); err == nil {
			h.opts.Catalog.Replace(next)
		}
		var operation any
		if req.Install {
			operation = h.discoveryInstall(added.Model.ModelID)
		}
		files := []map[string]any{}
		for _, f := range added.Entry.Weights {
			files = append(files, map[string]any{"path": f.Path, "sizeBytes": f.Size, "sha256": f.SHA256})
		}
		httpx.JSON(w, 200, map[string]any{"modelId": added.Model.ModelID, "displayName": added.Model.Name(), "kind": added.Entry.Kind,
			"repo": added.Entry.Repo, "revision": added.Entry.Revision, "files": files, "replaced": added.Replaced, "operation": operation})
	}
}

// discoveryInstall starts an install and reports its operation, or a failed
// one with the reason (TS deps.install).
func (h *Handler) discoveryInstall(modelID string) any {
	op, err := h.runManagement(modelID, "install")
	if err != nil {
		var me *mgmtError
		if errors.As(err, &me) {
			return map[string]any{"action": "install", "state": "failed", "detail": me.message}
		}
		return map[string]any{"action": "install", "state": "failed", "detail": err.Error()}
	}
	return op
}

// hubTokenOr is the saved token, else HF_TOKEN.
func hubTokenOr(saved string) string {
	if saved != "" {
		return saved
	}
	return os.Getenv("HF_TOKEN")
}
