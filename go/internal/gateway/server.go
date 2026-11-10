// Package gateway is LocalBase's HTTP surface: the OpenAI-compatible API,
// admin routes, and the mount point for the browser UI.
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/admin/keys"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/admin/models"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/admin/records"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/browser"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/runs"
	"github.com/timsexperiments/LocalBase/go/internal/library"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/otlp"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/upgrade"
	"github.com/timsexperiments/LocalBase/go/internal/usage"
	"github.com/timsexperiments/LocalBase/go/internal/version"
	"github.com/timsexperiments/LocalBase/go/internal/video"
	"github.com/timsexperiments/LocalBase/go/internal/voices"
)

// Server holds what handlers need. Construct with New.
type Server struct {
	// Placement is the CPU/GPU placement source (DESIGN §20; nil: no API).
	Placement *runtime.HostPlacement
	Store     *store.Store
	Catalog   *catalog.Catalog
	Auth      *auth.Resolver
	LLM       *runtime.LLM
	Log       *slog.Logger
	NoAuth    bool
	MemoryGB  int
	Root      *root.Root
	Memory    *memory.Controller
	Browser   *auth.Browser
	// Library joins the catalog, imports and backend selections (nil in
	// unit tests that need neither).
	Library *library.Library
	// STT, TTS and Image supervise the media modalities (nil: route disabled).
	STT   *runtime.Service
	TTS   *runtime.Service
	Image *runtime.Service
	// Video runs video jobs on VideoJobs (nil: no video runtime).
	Video     *runtime.Service
	VideoJobs *video.Manager
	// Voices stores custom voice profiles (nil: the routes are disabled).
	Voices *voices.Store
	// OnReady runs whenever readiness reports ready (a managed gateway
	// acknowledges its startup settings, TS acknowledgeStaticConfiguration).
	OnReady func()
	// Otel exports traces (nil: disabled).
	Otel *otlp.Runtime
	// Limits bounds what a request can cost (zero values take the defaults).
	Limits httpx.Limits
	// Usage keeps hourly totals (DESIGN §32; nil: not recorded, and the route is disabled).
	Usage *usage.Recorder
	// Updates is the optional daily check for a newer version (DESIGN §33; nil: the route is disabled).
	Updates *upgrade.Checker

	// models serves model management, discovery, imports and the /_localbase/models routes.
	models *models.Handler
	// records serves the host, computer name, usage, update and voices routes.
	records *records.Handler
	// infer serves the /v1 inference routes.
	infer *Handler
	// keys serves the key and access APIs, and the Hugging Face token.
	keys *keys.Handler
	// speechBusyFor is how long a busy tts-server is waited out; 0 is speechTimeout (tests).
	speechBusyFor time.Duration
	// unloadWait is how long an unload waits for in-flight requests; 0 is 30 s (tests).
	unloadWait time.Duration
	// relayBodyLimit and streamIdleLimit are the relay's limits; 0 is the default (tests).
	relayBodyLimit  int
	streamIdleLimit time.Duration

	// runs are the UI's detached runs (DESIGN §21).
	runs *runs.Manager
	// anonLog keeps what requests with no credential write to the log small.
	anonLog *obs.Sampler
	// anonRefused counts the 401s and 403s for those requests since start, logged or not.
	// anonAdminRefused counts the changes among them refused on an audited route, which
	// aren't audit events. Each summary carries both.
	anonRefused, anonAdminRefused atomic.Int64
	// refusedAudit keeps refused changes small in the audit file. Any key can be refused, and the
	// same file says who changed what.
	refusedAudit *obs.Sampler
	// changeAudit limits the changes on routes that keys:manage and access:manage don't guard, whose
	// holders could otherwise push the record of who changed keys and access out of the file.
	changeAudit *changeLimiter
	// UI handles /app and / when set.
	UI http.Handler
	// LoginPages renders local sign-in's pages (DESIGN §28); nil in tests without a UI.
	LoginPages browser.LoginPages
	// access serves the browser-owned paths and local sign-in.
	access *browser.Access

	started  time.Time
	stopping atomic.Bool
	mux      *http.ServeMux
	routes   []string // every registered pattern, for the OpenAPI drift test

	// RunMemoryLimit and RunsMemoryLimit are what a detached chat run, and all retained runs
	// together, may hold in memory; 0 is the default.
	RunMemoryLimit, RunsMemoryLimit int64
	// RunMaxDuration is how long a detached run may take before it is interrupted; 0 is the default
	// (15 minutes). No flag or setting sets it: tests do.
	RunMaxDuration time.Duration
	// Overrides applies what only this process has (the environment, serve's flags) to a saved
	// configuration, before it is published to the runtimes.
	Overrides func(*store.Config)
	// LogOutages reports logs that can't rotate or be written, for doctor and status.
	LogOutages func() []obs.LogOutage
}

// What requests with no credential may write to the log.
const (
	anonymousLogEvery = time.Minute
	anonymousLogBurst = 20
	refusedAuditEvery = time.Minute
	refusedAuditBurst = 20
)

func New(s *Server) *Server {
	s.started = time.Now()
	s.Limits = s.Limits.WithDefaults()
	rootDir := ""
	if s.Root != nil {
		rootDir = s.Root.Dir
	}
	s.infer = NewHandler(s.inferOptions())
	s.records = records.NewHandler(s.recordsOptions())
	s.models = models.NewHandler(s.modelsOptions())
	runsDir := ""
	if rootDir != "" {
		runsDir = filepath.Join(rootDir, "runs")
	}
	s.runs = runs.NewManager(s.runOptions(runsDir))
	s.anonLog = &obs.Sampler{Every: anonymousLogEvery, Burst: anonymousLogBurst}
	s.refusedAudit = &obs.Sampler{Every: refusedAuditEvery, Burst: refusedAuditBurst}
	s.changeAudit = newChangeLimiter()
	go s.runs.Janitor()
	mux := http.NewServeMux()
	s.access = browser.NewAccess(s.accessOptions(mux, rootDir))
	s.keys = keys.NewHandler(s.keysOptions(rootDir))
	for _, rt := range s.routeTable() {
		mux.Handle(rt.pattern, rt.handler)
		s.routes = append(s.routes, rt.pattern)
	}
	s.mux = mux
	return s
}

// runOptions is what the run manager needs from this server.
func (s *Server) runOptions(dir string) runs.Options {
	return runs.Options{Handler: s, Owner: s.runOwner, Log: func() *slog.Logger { return s.Log }, Otel: func() *otlp.Runtime { return s.Otel },
		Dir: dir, MaxHeld: s.RunMemoryLimit, MaxHeldTotal: s.RunsMemoryLimit, MaxDuration: s.RunMaxDuration}
}

// accessOptions is what the browser access layer needs from this server, forwarding to mux.
func (s *Server) accessOptions(mux http.Handler, rootDir string) browser.Options {
	return browser.Options{Handler: mux, Store: s.Store, Log: func() *slog.Logger { return s.Log },
		Browser: func() *auth.Browser { return s.Browser }, NoAuth: func() bool { return s.NoAuth },
		Pages: func() browser.LoginPages { return s.LoginPages }, RootDir: rootDir, Allowed: allowedUIRoute,
		Caller: s.principal, LookupFailed: s.lookupFailed}
}

// keysOptions is what the key and access APIs need from this server.
func (s *Server) keysOptions(rootDir string) keys.Options {
	return keys.Options{Store: s.Store, RootDir: rootDir, LLM: s.LLM, Browser: func() *auth.Browser { return s.Browser },
		Caller: s.principal}
}

// modelsOptions is what model management needs from this server.
func (s *Server) modelsOptions() models.Options {
	return models.Options{Store: s.Store, Root: s.Root, Catalog: s.Catalog, Placement: s.Placement, Memory: s.Memory, MemoryGB: s.MemoryGB,
		Runtimes: func() runtime.Set {
			return runtime.Set{LLM: s.LLM, STT: s.STT, TTS: s.TTS, Image: s.Image, Video: s.Video}
		},
		Log: func() *slog.Logger { return s.Log }, NoAuth: func() bool { return s.NoAuth },
		Library: func() *library.Library { return s.Library }, Overrides: func() func(*store.Config) { return s.Overrides },
		Caller: s.principal, LocalCLI: s.localCLI, AuditActor: s.auditActor, ImportedTask: s.importedTask,
		ReportReads: s.reportReads, AdmissionAPI: s.records.AdmissionAPI,
		UnloadWait: func() time.Duration { return s.unloadWait }}
}

// StopManagement lets an accepted install finish within grace, then
// interrupts it; the interruption is recorded, and the partial download
// stays for the next install to resume.
func (s *Server) StopManagement(grace time.Duration) { s.models.StopManagement(grace) }

// recordsOptions is what the host and records routes need from this server.
func (s *Server) recordsOptions() records.Options {
	return records.Options{Store: s.Store, Catalog: s.Catalog, Memory: s.Memory, Usage: s.Usage, Updates: s.Updates, VideoJobs: s.VideoJobs,
		Runtimes: func() runtime.Set {
			return runtime.Set{LLM: s.LLM, STT: s.STT, TTS: s.TTS, Image: s.Image, Video: s.Video}
		},
		Voices: func() *voices.Store { return s.Voices }, Log: func() *slog.Logger { return s.Log }, NoAuth: func() bool { return s.NoAuth },
		Caller: s.principal, LocalCLI: s.localCLI, UsagePrincipal: s.usagePrincipal, Unmatched: s.unmatched,
		ActiveRuns: func() int {
			if s.runs == nil {
				return 0
			}
			return s.runs.ActiveCount()
		}}
}

// inferOptions is what the /v1 inference routes need from this server.
func (s *Server) inferOptions() Options {
	return Options{Store: s.Store, Root: s.Root, Catalog: s.Catalog, MemoryGB: s.MemoryGB,
		Runtimes: func() runtime.Set {
			return runtime.Set{LLM: s.LLM, STT: s.STT, TTS: s.TTS, Image: s.Image, Video: s.Video}
		},
		Log: func() *slog.Logger { return s.Log }, Otel: func() *otlp.Runtime { return s.Otel }, NoAuth: func() bool { return s.NoAuth },
		Library: func() *library.Library { return s.Library }, ImportedTask: s.importedTask, ReportReads: s.reportReads,
		Voices: func() *voices.Store { return s.Voices }, SpeechBusyFor: func() time.Duration { return s.speechBusyFor },
		RelayBodyLimit: func() int { return s.relayBodyLimit }, StreamIdleLimit: func() time.Duration { return s.streamIdleLimit },
		VideoJobs: s.VideoJobs, Guard: s.guard, Unmatched: s.unmatched, RecordVideoJob: s.recordVideoJob}
}

// BrowserPrincipal resolves the signed-in browser identity of a request.
func (s *Server) BrowserPrincipal(r *http.Request) (*auth.Principal, *auth.Identity) {
	return s.access.Principal(r)
}

// StopRuns interrupts active runs (gateway_restart) and ends their event
// streams; serve calls it before the HTTP server drains.
func (s *Server) StopRuns(wait time.Duration) { s.runs.Shutdown(wait) }

// Stopping makes health and readiness report the gateway is shutting down.
func (s *Server) Stopping() { s.stopping.Store(true) }

// LogHeldBack writes what has been counted and not yet said. It's for once requests have stopped:
// during a run, only a later request brings a summary out.
func (s *Server) LogHeldBack() {
	if s.Browser != nil {
		s.Browser.LogHeldBack()
	}
	held := s.anonLog.Drain(time.Now())
	if held > 0 || s.anonRefused.Load() > 0 || s.anonAdminRefused.Load() > 0 {
		s.anonymousSummary(held)
	}
	s.changeSummary(s.changeAudit.drain(time.Now()))
	if held := s.refusedAudit.Drain(time.Now()); held > 0 {
		s.Log.Warn("Refused changes were counted rather than logged.", "event", "audit.refused-summary", "held_back", held)
	}
}

// anonymousSummary says how many requests with no credential were held back since the last one,
// with the totals since start. Totals alone aren't a warning.
func (s *Server) anonymousSummary(held int) {
	level := slog.LevelWarn
	if held == 0 {
		level = slog.LevelInfo
	}
	s.Log.Log(context.Background(), level, "Requests with no credential were counted rather than logged.", "event", "http.anonymous-summary", "category", "http",
		"count", held, "interval_seconds", int(s.anonLog.Every.Seconds()),
		"refused_total", s.anonRefused.Load(), "admin_refused_total", s.anonAdminRefused.Load())
}

// guard enforces a route permission. An empty permission admits any
// authenticated principal. API routes authenticate with API keys;
// browser sessions arrive with the auth milestone.
func (s *Server) guard(permission string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.NoAuth {
			next(w, r)
			return
		}
		if bp, ok := r.Context().Value(httpx.BrowserPrincipalKey{}).(*auth.Principal); ok {
			httpx.SetAuth(r, bp)
			if permission != "" && !bp.Can(permission) {
				httpx.Forbidden(w, r)
				return
			}
			next(w, r.WithContext(context.WithValue(r.Context(), httpx.PrincipalKey{}, bp)))
			return
		}
		p := s.resolveCredential(r)
		if !p.Authenticated() {
			httpx.Unauthorized(w, r)
			return
		}
		if permission != "" && !p.Can(permission) {
			httpx.Forbidden(w, r)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), httpx.PrincipalKey{}, p)))
	}
}

// --- request middleware -------------------------------------------------------

func newRequestID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	x := hex.EncodeToString(b)
	return "lbreq_" + x[0:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:]
}

// routeLabel names the matched route without raw path segments (#362).
func routeLabel(r *http.Request) (label, family string, segments int) {
	if r.Pattern != "" && r.Pattern != "/" {
		p := strings.ReplaceAll(strings.TrimSpace(strings.TrimPrefix(r.Pattern, r.Method)), "...}", "}")
		return strings.TrimSuffix(p, "{$}"), "", 0
	}
	path := r.URL.Path
	segments = min(16, len(strings.FieldsFunc(path, func(c rune) bool { return c == '/' })))
	for _, prefix := range []string{"/v1/models/", "/v1/videos/", "/v1/", "/_localbase/", "/app/api/"} {
		if strings.HasPrefix(path, prefix) {
			return "unmatched-route", prefix + "*", segments
		}
	}
	return "unmatched-route", "other", segments
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	id := newRequestID()
	w.Header().Set("X-Localbase-Request-Id", id)
	rec := &httpx.Recorder{ResponseWriter: w}
	rec.SendsWithin(s.Limits.WriteStall)
	if rec.Body = httpx.GuardBody(s.Limits, rec, r); rec.Body != nil {
		r.Body = rec.Body
		if s.NoAuth {
			rec.Body.Authenticated()
		}
	}
	// The server span (TS serve): named by the canonical route, parented by
	// an incoming traceparent, active for everything the request does.
	probe := r.Clone(r.Context())
	_, probe.Pattern = s.mux.Handler(probe)
	label, family, segments := routeLabel(probe)
	ctx, span := r.Context(), (*otlp.Span)(nil)
	if r.URL.Path != uiPollPath {
		ctx, span = s.Otel.Start(otlp.Extract(r.Context(), r.Header), r.Method+" "+label, otlp.KindServer,
			"http.request.method", r.Method, "http.route", label)
	}
	span.SetAttr("localbase.request_id", id)
	if family != "" {
		span.SetAttr("localbase.route.family", family)
		span.SetAttr("localbase.route.segments", segments)
	}
	r = r.WithContext(context.WithValue(auth.WithLookupState(ctx), httpx.RecKey{}, rec))
	startLabel := label
	defer func() {
		rec.ArmWrite() // what's still buffered leaves when this returns
		abort := false
		// Last of all, once usage, the log and the span are done: a panic after the answer started
		// resets the connection, so a client never takes half a stream for the whole of it, and
		// http.ErrAbortHandler aborts as net/http defines it.
		defer func() {
			if abort {
				panic(http.ErrAbortHandler)
			}
		}()
		if v := recover(); v != nil {
			s.Log.ErrorContext(r.Context(), "Handler panicked.", "event", "http.panic", "request_id", id, "error", slog.AnyValue(v).String())
			err, _ := v.(error)
			if rec.Status == 0 && !errors.Is(err, http.ErrAbortHandler) {
				httpx.ServiceUnavailable(rec, r, "Gateway")
			} else {
				abort = true
			}
		}
		defer span.End()
		label, family, segments := routeLabel(r)
		if r.Pattern != "" && label != startLabel { // forwarded inside the gateway (/app/api): name the route it reached
			span.SetName(r.Method + " " + label)
			span.SetAttr("http.route", label)
		}
		status := rec.FinalStatus()
		duration := time.Since(start)
		s.recordUsage(rec, label, status, duration)
		auth := s.authTelemetry(r, rec)
		span.SetAttr("http.response.status_code", status)
		span.SetAttr("localbase.http.response.duration_ms", float64(duration.Nanoseconds())/1e6)
		if status >= 500 || r.Context().Err() != nil {
			span.SetError("")
		}
		errType, errCode := rec.ErrorKind()
		if errType != "" {
			span.SetAttr("localbase.error.type", errType)
		}
		if errCode != "" {
			span.SetAttr("localbase.error.code", errCode)
			span.SetAttr("error.type", errCode)
		}
		for i := 0; i < len(auth); i += 2 {
			span.SetAttr("localbase.auth."+strings.TrimPrefix(auth[i].(string), "auth_"), auth[i+1])
		}
		now := time.Now()
		if n := s.anonLog.Flush(now); n > 0 {
			s.anonymousSummary(n)
		}
		if quietRoute(label, r.URL.Path) && status < 400 {
			return // successful probes and polls are not logged
		}
		anonymous := !s.NoAuth && status < 500 && (rec.Auth == nil || rec.Auth.Kind == "anonymous")
		if anonymous && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
			s.anonRefused.Add(1)
		}
		if anonymous && !s.anonLog.Allow(now) {
			return // anyone can send these, so a flood can't be what fills the log
		}
		msg := r.Method + " " + label + " -> " + strconv.Itoa(status)
		attrs := []any{"event", "http.request", "category", "http", "request_id", id,
			slog.Group("http", "method", r.Method, "path", label, "status", status,
				"durationMs", float64(duration.Microseconds())/1000)}
		if family != "" {
			msg += " (near " + family + ", " + strconv.Itoa(segments) + " segments)"
			attrs = append(attrs, "route_family", family, "route_segments", segments)
		}
		if errType != "" {
			attrs = append(attrs, "error_type", errType)
		}
		if errCode != "" {
			attrs = append(attrs, "error_code", errCode)
		}
		attrs = append(attrs, auth...)
		level := slog.LevelInfo
		switch {
		case status >= 500:
			level = slog.LevelError
		case status == 401 && label == "/app/session":
		case status >= 400:
			level = slog.LevelWarn
		}
		s.Log.Log(r.Context(), level, msg, attrs...)
	}()
	if s.NoAuth {
		if why := noAuthRefusal(r); why != "" {
			httpx.Error(rec, r, 403, "invalid_request_error", "no_auth_request_refused", why, nil)
			return
		}
	}
	// No method is exempt from authentication. This gateway sends no CORS
	// headers, so OPTIONS has no use outside the browser layer's own paths: it
	// is refused before any handler, as TS does.
	if r.Method == http.MethodOptions && !browser.IsUIAccessPath(r.URL.Path) {
		r.Pattern = probe.Pattern
		httpx.Forbidden(rec, r)
		return
	}
	s.mux.ServeHTTP(rec, r)
	if rec.Discarding() {
		rec.TimeoutAnswer = true
		httpx.RequestTimedOut(rec)
	}
}

// authTelemetry describes who a request acted as, without credentials (TS
// GatewayRequestAuth.telemetry): disabled for public routes and no-auth
// gateways, invalid when a credential was presented but not accepted.
func (s *Server) authTelemetry(r *http.Request, rec *httpx.Recorder) []any {
	p := rec.Auth
	switch {
	case auth.LookupFailed(r.Context()):
		return []any{"auth_outcome", "unavailable"}
	case s.NoAuth || p == nil:
		return []any{"auth_outcome", "disabled"}
	case p.Kind == "api-key":
		return []any{"auth_outcome", "authenticated", "auth_principal_id", p.ID, "auth_principal_name", p.Name, "auth_source", "stored_key"}
	case p.Kind == "environment":
		return []any{"auth_outcome", "authenticated", "auth_principal_id", "environment", "auth_source", "environment"}
	case p.Kind == "browser-session":
		return []any{"auth_outcome", "authenticated", "auth_principal_id", p.OwnerID(), "auth_source", "browser_session"}
	}
	presented := (s.Auth != nil && s.Auth.Token(r) != "") || r.Header.Get("Cookie") != "" || r.Header.Get("Cf-Access-Jwt-Assertion") != ""
	if presented {
		return []any{"auth_outcome", "invalid"}
	}
	return []any{"auth_outcome", "missing"}
}

func (s *Server) unmatched(w http.ResponseWriter, r *http.Request) {
	if !s.NoAuth {
		if p := s.resolveCredential(r); !p.Authenticated() {
			httpx.Unauthorized(w, r)
			return
		}
	}
	httpx.RouteDisabled(w, r)
}

// --- health ------------------------------------------------------------------

// modality reports one runtime's configuration, lifecycle state and whether
// a request could enter its queue now.
func (s *Server) modality(kind string) (configured bool, state string, admits bool) {
	if kind == "llm" {
		snap := s.LLM.Snapshot()
		configured = snap.State != "disabled"
		return configured, snap.State, configured && snap.Accepting && snap.Waiting < snap.Capacity
	}
	cfg := s.LLM.Config()
	if len(cfg.Selected[kind]) == 0 || cfg.Active[kind] == "" {
		return false, "disabled", false
	}
	svc := map[string]*runtime.Service{"stt": s.STT, "tts": s.TTS, "image": s.Image, "video": s.Video}[kind]
	if svc == nil {
		// A disabled service (--no-x) admits nothing.
		return true, "idle", false
	}
	snap := svc.Snapshot()
	return true, snap.State, snap.Accepting && snap.Waiting < snap.Capacity
}

func (s *Server) modalityHealth() map[string]any {
	out := map[string]any{}
	for _, kind := range store.Kinds {
		configured, state, _ := s.modality(kind)
		out[kind] = map[string]any{"configured": configured, "state": state}
	}
	return out
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{"status": "ok", "version": version.Version,
		"uptimeSeconds": int(time.Since(s.started).Seconds()), "modalities": s.modalityHealth()}
	status := 200
	if s.stopping.Load() {
		body["status"], body["error"], status = "error", "gateway_stopping", 503
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		return
	}
	httpx.JSON(w, status, body)
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	admitting := []string{}
	for _, kind := range store.Kinds {
		if _, _, admits := s.modality(kind); admits {
			admitting = append(admitting, kind)
		}
	}
	body := map[string]any{"status": "ready", "reason": "request_admission_available", "modalities": admitting}
	status := 200
	switch {
	case s.stopping.Load():
		body, status = map[string]any{"status": "unready", "reason": "gateway_stopping", "modalities": []string{}}, 503
	case len(admitting) == 0:
		body, status = map[string]any{"status": "unready", "reason": "no_request_admission", "modalities": []string{}}, 503
	}
	if status == 200 && s.OnReady != nil {
		s.OnReady()
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	httpx.JSON(w, status, body)
}

// Ready reports whether some modality admits requests and the gateway is
// not stopping.
func (s *Server) Ready() bool {
	if s.stopping.Load() {
		return false
	}
	for _, kind := range store.Kinds {
		if _, _, admits := s.modality(kind); admits {
			return true
		}
	}
	return false
}

// instance proves this gateway's identity to a CLI that knows the instance
// token from the lock record (guards against PID reuse).
// defaultModel reports the effective default chat model to the local CLI
// (`status`), behind the instance token like /_localbase/instance: model
// ids stay off the unauthenticated /health (DESIGN §18).
func (s *Server) defaultModel(w http.ResponseWriter, r *http.Request) {
	if !httpx.InstanceTokenMatches(s.Root.Instance(), r) || s.LLM == nil {
		w.WriteHeader(404)
		return
	}
	c := s.LLM.DefaultModel(false)
	httpx.JSON(w, 200, map[string]any{"modelId": c.ModelID, "preferredModelId": c.Preferred, "reason": c.Reason,
		"defaults": s.LLM.Config().DefaultLLMList()})
}

func (s *Server) instance(w http.ResponseWriter, r *http.Request) {
	o := s.Root.Instance()
	if !httpx.InstanceTokenMatches(o, r) {
		w.WriteHeader(404)
		return
	}
	out := map[string]any{"instanceId": o.InstanceID, "rootHash": o.RootHash}
	if s.LogOutages != nil {
		if outages := s.LogOutages(); len(outages) > 0 {
			out["logOutages"] = outages // Only then, so the usual answer doesn't change.
		}
	}
	httpx.JSON(w, 200, out)
}
