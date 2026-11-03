package gateway

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/usage"
)

// Usage and activity (DESIGN §32): hourly totals, never content. Requests are counted
// here, at the end of ServeHTTP, so a request refused anywhere counts once, and a detached
// run counts as its inner request.

// usageRoutes names what is counted, by the route that served it.
var usageRoutes = map[string]string{
	"/v1/chat/completions":     "chat",
	"/v1/embeddings":           "embeddings",
	"/v1/systemone":            "decision",
	"/v1/audio/speech":         "speech",
	"/v1/audio/transcriptions": "transcription",
	"/v1/audio/translations":   "translation",
	"/v1/images/generations":   "image",
	"/v1/images/edits":         "image-edit",
	"/v1/videos":               "video",
}

// usageNote is what a handler knows that the middleware cannot see.
type usageNote struct {
	model     string
	trusted   bool // the model came from a lease, so the gateway resolved it
	outcome   string
	status    int
	in, out   int64
	hasTokens bool
	queue     time.Duration
}

func noteFrom(r *http.Request) *usageNote {
	if rec, ok := r.Context().Value(recKey{}).(*recorder); ok {
		return &rec.usage
	}
	return nil
}

// usagePrincipal says who to count a request under. A browser identity is its opaque owner
// hash and nothing else: never an email or a name.
func (s *Server) usagePrincipal(p *auth.Principal) (kind, id string, ok bool) {
	if s.NoAuth {
		return "no-auth", "no-auth", true
	}
	if p == nil || !p.Authenticated() {
		return "", "", false
	}
	switch p.Kind {
	case "api-key":
		return "api-key", p.ID, true
	case "environment":
		return "environment", "environment", true
	case "browser-session":
		if p.ID == auth.LocalOwnerID {
			return "local", "local", true
		}
		if strings.HasPrefix(p.ID, "browser:") {
			return "browser", p.ID, true
		}
		sum := sha256.Sum256([]byte(p.ID))
		return "browser", "browser:" + hex.EncodeToString(sum[:]), true
	}
	return "", "", false
}

// knownUsageModel: only an id the gateway knows is stored, so a string a client typed can
// never land in the table.
func (s *Server) knownUsageModel(id string) bool {
	if id == "" {
		return false
	}
	if s.Catalog != nil && s.Catalog.ByID(id) != nil {
		return true
	}
	return s.importedTask(id) != ""
}

// recordUsage counts a finished request. It never fails one.
func (s *Server) recordUsage(rec *recorder, label string, status int, d time.Duration) {
	if s.Usage == nil {
		return
	}
	route, counted := usageRoutes[label]
	if !counted {
		return
	}
	kind, id, ok := s.usagePrincipal(rec.auth)
	if !ok {
		return
	}
	if route == "video" && status == http.StatusAccepted {
		return // the job counts once, when it ends
	}
	n := rec.usage
	switch n.outcome {
	case "error": // a stream that fails after it started has already sent 200
		status = n.status
		if status < 400 {
			status = http.StatusBadGateway
		}
	case "cancelled":
		status = 499
	}
	model := n.model
	if !n.trusted && !s.knownUsageModel(model) {
		model = ""
	}
	s.Usage.Add(usage.Event{Kind: kind, Principal: id, Route: route, Model: model, Status: status, Duration: d, QueueWait: n.queue,
		InputTokens: n.in, OutputTokens: n.out, HasTokens: n.hasTokens})
}

// recordVideoJob counts a job when it ends: the only place a job is counted.
func (s *Server) recordVideoJob(p *auth.Principal, model string, status int, d time.Duration) {
	if s.Usage == nil {
		return
	}
	kind, id, ok := s.usagePrincipal(p)
	if !ok {
		return
	}
	s.Usage.Add(usage.Event{Kind: kind, Principal: id, Route: "video", Model: model, Status: status, Duration: d})
}

// --- the API --------------------------------------------------------------------------

func (s *Server) usageAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		s.usageRead(w, r)
	case http.MethodPost:
		s.usageChange(w, r)
	default:
		methodNotAllowed(w, r, "GET, POST")
	}
}

func (s *Server) usageRead(w http.ResponseWriter, r *http.Request) {
	p := s.principal(r)
	if !p.Authenticated() {
		unauthorized(w, r)
		return
	}
	if s.Usage == nil {
		notConfigured(w, r, "Usage")
		return
	}
	query := r.URL.Query()
	scope := query.Get("scope")
	switch scope {
	case "":
		scope = "self"
	case "self", "all":
	default:
		badRequest(w, r, "scope must be self or all.", strp("scope"))
		return
	}
	_, own, _ := s.usagePrincipal(p)
	principal := query.Get("principal")
	if scope == "all" || (principal != "" && principal != own) {
		if !s.NoAuth && !usage.CanSeeAll(p.Can) {
			forbidden(w, r)
			return
		}
	}
	q := usage.Query{Principal: principal, Model: query.Get("model"), Route: query.Get("route"), Group: query["group"]}
	if scope == "self" {
		// A caller's own view stays its own, whatever it can see otherwise.
		if principal != "" && principal != own {
			badRequest(w, r, "scope=self covers your own usage. Use scope=all to name another principal.", strp("principal"))
			return
		}
		q.Principal = own
	}
	for _, g := range q.Group {
		if g != "principal" && g != "model" && g != "route" {
			badRequest(w, r, "group must be principal, model or route.", strp("group"))
			return
		}
	}
	if q.Route != "" && !slices.Contains(usage.Routes, q.Route) {
		badRequest(w, r, "route is not one of the routes usage counts.", strp("route"))
		return
	}
	var err error
	if q.From, q.To, err = usage.ParseRange(query.Get("range"), query.Get("from"), query.Get("to"), time.Now()); err != nil {
		badRequest(w, r, err.Error()+".", strp("range"))
		return
	}
	report, err := s.Usage.Report(r.Context(), q)
	if err != nil {
		s.Log.Error("Could not read usage.", "event", "usage.read-failed", "error", err.Error())
		serviceUnavailable(w, r, "Usage")
		return
	}
	report.Scope = scope
	writeJSON(w, http.StatusOK, report)
}

// localCLI: this machine's CLI, holding the token from the root's lock file, on a request that
// passes the loopback rule (DESIGN §32.4). It is good for changes only.
func (s *Server) localCLI(r *http.Request) bool {
	if s.Root == nil || auth.IsBrowser(r) || !auth.LocalRequest(r) {
		return false
	}
	return instanceTokenMatches(s.Root.Instance(), r)
}

// instanceTokenMatches says whether a request carries this instance's token. Every route behind the
// token asks here, so the comparison takes as long whatever was sent, and no token can match "none".
func instanceTokenMatches(o *root.Owner, r *http.Request) bool {
	sent := r.Header.Get("X-Localbase-Instance-Token")
	return o != nil && o.InstanceToken != "" && subtle.ConstantTimeCompare([]byte(sent), []byte(o.InstanceToken)) == 1
}

func (s *Server) usageChange(w http.ResponseWriter, r *http.Request) {
	actorKind, actorID := "cli", ""
	if r.Header.Get("X-Localbase-Instance-Token") != "" {
		if !s.localCLI(r) {
			unauthorized(w, r)
			return
		}
	} else {
		p := s.principal(r)
		if !p.Authenticated() {
			unauthorized(w, r)
			return
		}
		if !s.NoAuth && !usage.CanChange(p.Can) {
			forbidden(w, r)
			return
		}
		actorKind, actorID, _ = s.usagePrincipal(p)
	}
	if s.Usage == nil {
		notConfigured(w, r, "Usage")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
	if err != nil {
		badRequest(w, r, "Invalid usage request.", nil)
		return
	}
	if len(body) > 4096 {
		payloadTooLarge(w, r)
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
		badRequest(w, r, "Invalid usage request.", nil)
		return
	}
	ctx := r.Context()
	cleared := false
	switch in.Action {
	case "clear":
		if in.Days != nil || in.Enabled != nil {
			badRequest(w, r, "clear takes only before.", nil)
			return
		}
		var before *time.Time
		if in.Before != nil {
			t, perr := time.Parse(time.RFC3339, *in.Before)
			if perr != nil {
				badRequest(w, r, "before must be an RFC 3339 time.", strp("before"))
				return
			}
			before = &t
		}
		err = s.Usage.Clear(ctx, before)
		cleared = true
	case "set-retention":
		if in.Days == nil || in.Before != nil || in.Enabled != nil || *in.Days != float64(int(*in.Days)) {
			badRequest(w, r, "set-retention takes days, a whole number.", strp("days"))
			return
		}
		err = s.Usage.SetRetention(ctx, int(*in.Days))
		var invalid *usage.InvalidRetention
		if errors.As(err, &invalid) {
			badRequest(w, r, invalid.Error()+".", strp("days"))
			return
		}
	case "set-recording":
		if in.Enabled == nil || in.Before != nil || in.Days != nil {
			badRequest(w, r, "set-recording takes enabled.", strp("enabled"))
			return
		}
		err = s.Usage.SetRecording(ctx, *in.Enabled)
	default:
		badRequest(w, r, "action must be clear, set-retention or set-recording.", strp("action"))
		return
	}
	if err != nil {
		s.Log.Error("Could not change usage.", "event", "usage.change-failed", "error", err.Error())
		serviceUnavailable(w, r, "Usage")
		return
	}
	s.Log.Info("Administration change.", "event", "audit.usage-changed", "action", in.Action, "actor_kind", actorKind, "actor_id", actorID)
	recording, days := s.Usage.Settings()
	writeJSON(w, http.StatusOK, map[string]any{"object": "localbase.usage.settings", "recording": recording, "retentionDays": days, "cleared": cleared})
}
