// Package gateway is LocalBase's HTTP surface: the OpenAI-compatible API,
// admin routes, and the mount point for the browser UI.
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

const Version = "0.2.0-go"

// Server holds what handlers need. Construct with New.
type Server struct {
	Store    *store.Store
	Catalog  *catalog.Catalog
	Auth     *auth.Resolver
	LLM      *runtime.LLM
	Log      *slog.Logger
	NoAuth   bool
	MemoryGB int
	Root     *root.Root
	// UI handles /app and / when set.
	UI http.Handler

	started  time.Time
	stopping atomic.Bool
	mux      *http.ServeMux
}

func New(s *Server) *Server {
	s.started = time.Now()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.health)
	mux.HandleFunc("/health/ready", s.ready)
	mux.HandleFunc("/v1/models", s.guard("models:read", s.listModels))
	mux.HandleFunc("/v1/models/{model...}", s.guard("models:read", s.retrieveModel))
	mux.HandleFunc("/v1/chat/completions", s.guard("inference:chat", s.chat))
	mux.HandleFunc("/v1/embeddings", s.guard("inference:embeddings", s.embeddings))
	mux.HandleFunc("/v1/systemone", s.guard("inference:decision", s.systemone))
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
	r = r.WithContext(context.WithValue(r.Context(), recKey{}, rec))
	defer func() {
		if v := recover(); v != nil {
			s.Log.Error("Handler panicked.", "event", "http.panic", "request_id", id, "error", slog.AnyValue(v).String())
			if rec.status == 0 {
				serviceUnavailable(rec, r, "Gateway")
			}
		}
		label, family, segments := routeLabel(r)
		status := rec.status
		if status == 0 {
			status = 200
		}
		if (label == "/health" || label == "/health/ready") && status < 400 {
			return // successful probes are not logged
		}
		msg := r.Method + " " + label + " -> " + strconv.Itoa(status)
		attrs := []any{"event", "http.request", "category", "http", "request_id", id,
			slog.Group("http", "method", r.Method, "path", label, "status", status,
				"durationMs", float64(time.Since(start).Microseconds())/1000)}
		if family != "" {
			msg += " (near " + family + ", " + strconv.Itoa(segments) + " segments)"
			attrs = append(attrs, "route_family", family, "route_segments", segments)
		}
		switch {
		case s.NoAuth:
			attrs = append(attrs, "auth_outcome", "disabled")
		case rec.auth != nil && rec.auth.Authenticated():
			src := map[string]string{"api-key": "stored_key", "environment": "environment"}[rec.auth.Kind]
			attrs = append(attrs, "auth_outcome", "authenticated", "auth_principal_id", rec.auth.OwnerID(),
				"auth_principal_name", rec.auth.Name, "auth_source", src)
		case rec.auth != nil:
			attrs = append(attrs, "auth_outcome", "unauthenticated")
		}
		s.Log.Info(msg, attrs...)
	}()
	s.mux.ServeHTTP(rec, r)
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

func (s *Server) modalityHealth() map[string]any {
	cfg := s.LLM.Config()
	out := map[string]any{}
	running, _, _ := s.LLM.Running()
	for _, kind := range store.Kinds {
		configured := len(runtime.ConfiguredIDs(cfg, kind)) > 0
		state := "disabled"
		if configured {
			state = "idle"
		}
		if kind == "llm" && running != "" {
			state = "running"
		}
		if kind != "llm" && configured {
			state = "disabled" // served from milestone 4
			configured = false
		}
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
	body := map[string]any{"status": "ready", "reason": "request_admission_available", "modalities": []string{"llm"}}
	status := 200
	if s.stopping.Load() {
		body, status = map[string]any{"status": "unready", "reason": "gateway_stopping", "modalities": []string{}}, 503
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	writeJSON(w, status, body)
}
