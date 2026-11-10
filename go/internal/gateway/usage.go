package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
	"github.com/timsexperiments/LocalBase/go/internal/usage"
)

// --- the API --------------------------------------------------------------------------

func (h *Handler) UsageAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		h.usageRead(w, r)
	case http.MethodPost:
		h.usageChange(w, r)
	default:
		// Checked here, not by the route table's gate: the 405 carries this handler's Cache-Control.
		httpx.MethodNotAllowed(w, r, "GET, POST")
	}
}

func (h *Handler) usageRead(w http.ResponseWriter, r *http.Request) {
	p := h.opts.Caller(r)
	if !p.Authenticated() {
		httpx.Unauthorized(w, r)
		return
	}
	if h.opts.Usage == nil {
		httpx.NotConfigured(w, r, "Usage")
		return
	}
	query := r.URL.Query()
	scope := query.Get("scope")
	switch scope {
	case "":
		scope = "self"
	case "self", "all":
	default:
		httpx.BadRequest(w, r, "scope must be self or all.", httpx.StrPtr("scope"))
		return
	}
	_, own, _ := h.opts.UsagePrincipal(p)
	principal := query.Get("principal")
	if scope == "all" || (principal != "" && principal != own) {
		if !h.opts.NoAuth() && !usage.CanSeeAll(p.Can) {
			httpx.Forbidden(w, r)
			return
		}
	}
	q := usage.Query{Principal: principal, Model: query.Get("model"), Route: query.Get("route"), Group: query["group"]}
	if scope == "self" {
		// A caller's own view stays its own, whatever it can see otherwise.
		if principal != "" && principal != own {
			httpx.BadRequest(w, r, "scope=self covers your own usage. Use scope=all to name another principal.", httpx.StrPtr("principal"))
			return
		}
		q.Principal = own
	}
	for _, g := range q.Group {
		if g != "principal" && g != "model" && g != "route" {
			httpx.BadRequest(w, r, "group must be principal, model or route.", httpx.StrPtr("group"))
			return
		}
	}
	if q.Route != "" && !slices.Contains(usage.Routes, q.Route) {
		httpx.BadRequest(w, r, "route is not one of the routes usage counts.", httpx.StrPtr("route"))
		return
	}
	var err error
	if q.From, q.To, err = usage.ParseRange(query.Get("range"), query.Get("from"), query.Get("to"), time.Now()); err != nil {
		httpx.BadRequest(w, r, err.Error()+".", httpx.StrPtr("range"))
		return
	}
	report, err := h.opts.Usage.Report(r.Context(), q)
	if err != nil {
		h.opts.Log().Error("Could not read usage.", "event", "usage.read-failed", "error", err.Error())
		httpx.ServiceUnavailable(w, r, "Usage")
		return
	}
	report.Scope = scope
	httpx.JSON(w, http.StatusOK, report)
}

func (h *Handler) usageChange(w http.ResponseWriter, r *http.Request) {
	actorKind, actorID := "cli", ""
	if r.Header.Get("X-Localbase-Instance-Token") != "" {
		if !h.opts.LocalCLI(r) {
			httpx.Unauthorized(w, r)
			return
		}
	} else {
		p := h.opts.Caller(r)
		if !p.Authenticated() {
			httpx.Unauthorized(w, r)
			return
		}
		if !h.opts.NoAuth() && !usage.CanChange(p.Can) {
			httpx.Forbidden(w, r)
			return
		}
		actorKind, actorID, _ = h.opts.UsagePrincipal(p)
	}
	if h.opts.Usage == nil {
		httpx.NotConfigured(w, r, "Usage")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
	if err != nil {
		httpx.BadRequest(w, r, "Invalid usage request.", nil)
		return
	}
	if len(body) > 4096 {
		httpx.PayloadTooLarge(w, r)
		return
	}
	var in struct {
		Action  string   `json:"action"`
		Before  *string  `json:"before"`
		Days    *float64 `json:"days"`
		Enabled *bool    `json:"enabled"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		httpx.BadRequest(w, r, "Invalid usage request.", nil)
		return
	}
	ctx := r.Context()
	cleared := false
	switch in.Action {
	case "clear":
		if in.Days != nil || in.Enabled != nil {
			httpx.BadRequest(w, r, "clear takes only before.", nil)
			return
		}
		var before *time.Time
		if in.Before != nil {
			t, perr := time.Parse(time.RFC3339, *in.Before)
			if perr != nil {
				httpx.BadRequest(w, r, "before must be an RFC 3339 time.", httpx.StrPtr("before"))
				return
			}
			before = &t
		}
		err = h.opts.Usage.Clear(ctx, before)
		cleared = true
	case "set-retention":
		if in.Days == nil || in.Before != nil || in.Enabled != nil || *in.Days != float64(int(*in.Days)) {
			httpx.BadRequest(w, r, "set-retention takes days, a whole number.", httpx.StrPtr("days"))
			return
		}
		err = h.opts.Usage.SetRetention(ctx, int(*in.Days))
		var invalid *usage.InvalidRetention
		if errors.As(err, &invalid) {
			httpx.BadRequest(w, r, invalid.Error()+".", httpx.StrPtr("days"))
			return
		}
	case "set-recording":
		if in.Enabled == nil || in.Before != nil || in.Days != nil {
			httpx.BadRequest(w, r, "set-recording takes enabled.", httpx.StrPtr("enabled"))
			return
		}
		err = h.opts.Usage.SetRecording(ctx, *in.Enabled)
	default:
		httpx.BadRequest(w, r, "action must be clear, set-retention or set-recording.", httpx.StrPtr("action"))
		return
	}
	if err != nil {
		h.opts.Log().Error("Could not change usage.", "event", "usage.change-failed", "error", err.Error())
		httpx.ServiceUnavailable(w, r, "Usage")
		return
	}
	h.opts.Log().Info("Administration change.", "event", "audit.usage-changed", "action", in.Action, "actor_kind", actorKind, "actor_id", actorID)
	recording, days := h.opts.Usage.Settings()
	httpx.JSON(w, http.StatusOK, map[string]any{"object": "localbase.usage.settings", "recording": recording, "retentionDays": days, "cleared": cleared})
}
