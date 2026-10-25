// Package gateway is LocalBase's HTTP surface: the OpenAI-compatible API,
// admin routes, and the mount point for the browser UI.
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/library"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/otlp"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/video"
	"github.com/timsexperiments/LocalBase/go/internal/voices"
)

const Version = "0.2.0"

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

	mgmt *management
	// UI handles /app and / when set.
	UI http.Handler

	started  time.Time
	stopping atomic.Bool
	mux      *http.ServeMux
}

func New(s *Server) *Server {
	s.started = time.Now()
	rootDir := ""
	if s.Root != nil {
		rootDir = s.Root.Dir
	}
	s.mgmt = newManagement(rootDir)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.health)
	for _, p := range []string{"/app/login", "/oidc/callback", "/github/callback", "/app/login/magic", "/magic-link/callback",
		"/app/logout", "/app/session", "/app/api", "/app/api/"} {
		mux.HandleFunc(p, s.uiAccess)
	}
	mux.HandleFunc("/health/ready", s.ready)
	mux.HandleFunc("/_localbase/instance", s.instance)
	mux.HandleFunc("/_localbase/default-model", s.defaultModel)
	mux.HandleFunc("/_localbase/placement-state", s.placementRuntimeState)
	mux.HandleFunc("/v1/models", s.guard("models:read", s.listModels))
	mux.HandleFunc("/v1/models/{model...}", s.guard("models:read", s.retrieveModel))
	mux.HandleFunc("/v1/chat/completions", s.guard("inference:chat", s.chat))
	mux.HandleFunc("/v1/embeddings", s.guard("inference:embeddings", s.embeddings))
	mux.HandleFunc("/v1/systemone", s.guard("inference:decision", s.systemone))
	mux.HandleFunc("/v1/audio/transcriptions", s.guard("inference:transcription", s.transcription))
	mux.HandleFunc("/v1/audio/translations", s.guard("inference:transcription", s.transcription))
	mux.HandleFunc("/v1/audio/speech", s.guard("inference:speech", s.speech))
	mux.HandleFunc("/v1/images/generations", s.guard("inference:image", s.imageGeneration))
	mux.HandleFunc("/v1/images/edits", s.guard("inference:image", s.imageEdit))
	mux.HandleFunc("/v1/videos", s.guard("inference:video", s.videoCreate))
	mux.HandleFunc("/v1/videos/{job_id}", s.videoJobRoute("status"))
	mux.HandleFunc("/v1/videos/{job_id}/content", s.videoJobRoute("content"))
	mux.HandleFunc("/v1/videos/{job_id}/cancel", s.videoJobRoute("cancel"))
	mux.HandleFunc("/_localbase/models", s.guard("models:read", s.modelMetadata))
	mux.HandleFunc("/_localbase/models/{model_id...}", s.guard("models:read", s.modelMetadata))
	mux.HandleFunc("/_localbase/access-management", s.audited("access", s.accessManagement))
	mux.HandleFunc("/_localbase/api-keys", s.audited("api-key", s.apiKeys))
	mux.HandleFunc("/_localbase/hf-token", s.hfToken)
	mux.HandleFunc("/_localbase/backends", s.backendsAPI)
	mux.HandleFunc("/_localbase/model-backend", s.modelBackendAPI)
	mux.HandleFunc("/_localbase/model-management", s.modelManagementAPI)
	mux.HandleFunc("/_localbase/default-chat-models", s.defaultChatModelsAPI)
	mux.HandleFunc("/_localbase/model-discovery", s.modelDiscoveryAPI)
	mux.HandleFunc("/_localbase/model-imports", s.modelImportsAPI)
	mux.HandleFunc("/_localbase/auth-profiles", s.authProfilesAPI)
	mux.HandleFunc("/_localbase/voices", s.voicesAPI)
	mux.HandleFunc("/_localbase/voices/", s.voicesAPI)
	if s.UI != nil {
		mux.Handle("/app", s.UI)
		mux.Handle("/app/", s.UI)
		mux.Handle("/{$}", s.UI)
	}
	mux.HandleFunc("/", s.unmatched)
	s.mux = mux
	return s
}

// Stopping makes health and readiness report the gateway is shutting down.
func (s *Server) Stopping() { s.stopping.Store(true) }

type principalKey struct{}

// PrincipalFrom returns the request's resolved principal.
func PrincipalFrom(ctx context.Context) *auth.Principal {
	if p, ok := ctx.Value(principalKey{}).(*auth.Principal); ok {
		return p
	}
	return auth.Anonymous
}

// guard enforces a route permission. API routes authenticate with API keys;
// browser sessions arrive with the auth milestone.
func (s *Server) guard(permission string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next(w, r)
			return
		}
		if s.NoAuth {
			next(w, r)
			return
		}
		if bp, ok := r.Context().Value(browserPrincipalKey{}).(*auth.Principal); ok {
			if !bp.Can(permission) {
				forbidden(w, r)
				return
			}
			next(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, bp)))
			return
		}
		p, err := s.Auth.ResolveAPI(r.Context(), r)
		if err != nil {
			s.Log.Error("Credential lookup failed.", "event", "auth.lookup-failed", "error", err.Error())
			serviceUnavailable(w, r, "Authentication")
			return
		}
		setAuth(r, p)
		if !p.Authenticated() {
			unauthorized(w, r)
			return
		}
		if !p.Can(permission) {
			forbidden(w, r)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	}
}

// --- request middleware -------------------------------------------------------

type recorder struct {
	http.ResponseWriter
	status int
	auth   *auth.Principal
	// errBody is the start of an error response, read only for the error
	// envelope's type and code (never its message).
	errBody []byte
}

const errSniffBytes = 8 << 10

// errorKind is the error envelope's type and code ("" when the response is
// not one).
func (r *recorder) errorKind() (typ, code string) {
	var env struct {
		Error *struct {
			Type any `json:"type"`
			Code any `json:"code"`
		} `json:"error"`
	}
	if len(r.errBody) == 0 || json.Unmarshal(r.errBody, &env) != nil || env.Error == nil {
		return "", ""
	}
	if t, ok := env.Error.Type.(string); ok {
		typ = t
	}
	switch c := env.Error.Code.(type) {
	case string:
		code = c
	case float64:
		code = strconv.FormatFloat(c, 'f', -1, 64)
	}
	return typ, code
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = 200
	}
	if r.status >= 400 && len(r.errBody) < errSniffBytes {
		r.errBody = append(r.errBody, b[:min(len(b), errSniffBytes-len(r.errBody))]...)
	}
	return r.ResponseWriter.Write(b)
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

type recKey struct{}

func setAuth(r *http.Request, p *auth.Principal) {
	if rec, ok := r.Context().Value(recKey{}).(*recorder); ok {
		rec.auth = p
	}
}

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
	rec := &recorder{ResponseWriter: w}
	// The server span (TS serve): named by the canonical route, parented by
	// an incoming traceparent, active for everything the request does.
	probe := r.Clone(r.Context())
	_, probe.Pattern = s.mux.Handler(probe)
	label, family, segments := routeLabel(probe)
	ctx, span := s.Otel.Start(otlp.Extract(r.Context(), r.Header), r.Method+" "+label, otlp.KindServer,
		"http.request.method", r.Method, "http.route", label)
	span.SetAttr("localbase.request_id", id)
	if family != "" {
		span.SetAttr("localbase.route.family", family)
		span.SetAttr("localbase.route.segments", segments)
	}
	r = r.WithContext(context.WithValue(ctx, recKey{}, rec))
	defer func() {
		if v := recover(); v != nil {
			s.Log.ErrorContext(r.Context(), "Handler panicked.", "event", "http.panic", "request_id", id, "error", slog.AnyValue(v).String())
			if rec.status == 0 {
				serviceUnavailable(rec, r, "Gateway")
			}
		}
		defer span.End()
		label, family, segments := routeLabel(r)
		status := rec.status
		if status == 0 {
			status = 200
		}
		duration := time.Since(start)
		auth := s.authTelemetry(r, rec)
		span.SetAttr("http.response.status_code", status)
		span.SetAttr("localbase.http.response.duration_ms", float64(duration.Nanoseconds())/1e6)
		if status >= 500 || r.Context().Err() != nil {
			span.SetError("")
		}
		errType, errCode := rec.errorKind()
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
		if (label == "/health" || label == "/health/ready") && status < 400 {
			return // successful probes are not logged
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
	s.mux.ServeHTTP(rec, r)
}

// authTelemetry describes who a request acted as, without credentials (TS
// GatewayRequestAuth.telemetry): disabled for public routes and no-auth
// gateways, invalid when a credential was presented but not accepted.
func (s *Server) authTelemetry(r *http.Request, rec *recorder) []any {
	p := rec.auth
	switch {
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
		p, _ := s.Auth.ResolveAPI(r.Context(), r)
		setAuth(r, p)
		if !p.Authenticated() {
			unauthorized(w, r)
			return
		}
	}
	routeDisabled(w, r)
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
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, r, "GET, HEAD")
		return
	}
	body := map[string]any{"status": "ok", "version": Version,
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
	writeJSON(w, status, body)
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, r, "GET, HEAD")
		return
	}
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
	writeJSON(w, status, body)
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
	o := s.Root.Instance()
	if o == nil || r.Header.Get("X-Localbase-Instance-Token") != o.InstanceToken || s.LLM == nil {
		w.WriteHeader(404)
		return
	}
	c := s.LLM.DefaultModel(false)
	writeJSON(w, 200, map[string]any{"modelId": c.ModelID, "preferredModelId": c.Preferred, "reason": c.Reason,
		"defaults": s.LLM.Config().DefaultLLMList()})
}

func (s *Server) instance(w http.ResponseWriter, r *http.Request) {
	o := s.Root.Instance()
	if o == nil || r.Header.Get("X-Localbase-Instance-Token") != o.InstanceToken {
		w.WriteHeader(404)
		return
	}
	writeJSON(w, 200, map[string]string{"instanceId": o.InstanceID, "rootHash": o.RootHash})
}
