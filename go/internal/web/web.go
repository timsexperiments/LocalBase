// Package web is the server-rendered browser UI: html/template pages and
// fragments, htmx for interaction, Tailwind CSS compiled at build time.
// Handlers call the gateway's services directly; there is no JSON client
// and no /app/api mirror.
package web

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/library"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

//go:generate ../../tools/tailwind.sh

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

type Deps struct {
	Store   *store.Store
	Catalog *catalog.Catalog
	LLM     *runtime.LLM
	Auth    *auth.Resolver
	Memory  *memory.Controller
	NoAuth  bool
	RootDir string
	// Principal resolves the signed-in browser identity (nil when none).
	Principal func(*http.Request) *auth.Principal
	// SignInEnabled: browser access is configured (ui-access.json).
	SignInEnabled bool
	// LogoutURL is where a Cloudflare Access session signs out ("" otherwise).
	LogoutURL string
	// Library: models, imports and backend selection.
	Library *library.Library
}

type UI struct {
	d        Deps
	pages    map[string]*template.Template
	assetVer map[string]string
	mux      *http.ServeMux
}

func New(d Deps) *UI {
	u := &UI{d: d, assetVer: map[string]string{}}
	fs.WalkDir(staticFS, "static", func(p string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			b, _ := staticFS.ReadFile(p)
			sum := sha256.Sum256(b)
			u.assetVer[strings.TrimPrefix(p, "static/")] = hex.EncodeToString(sum[:6])
		}
		return nil
	})
	funcs := template.FuncMap{
		"asset": func(name string) string { return "/app/assets/" + name + "?v=" + u.assetVer[name] },
		"gb":    func(b int64) string { return fmt.Sprintf("%.1f GB", float64(b)/1e9) },
		"size": func(b int64) string {
			if b < 1e9 {
				return fmt.Sprintf("%.0f MB", float64(b)/1e6)
			}
			return fmt.Sprintf("%.1f GB", float64(b)/1e9)
		},
		"num": func(n int) string {
			s := fmt.Sprint(n)
			for i := len(s) - 3; i > 0; i -= 3 {
				s = s[:i] + "," + s[i:]
			}
			return s
		},
		"gib":  func(b int64) string { return fmt.Sprintf("%.1f GiB", float64(b)/float64(memory.GiB)) },
		"list": func(v ...string) []string { return v },
		"dict": func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				m[kv[i].(string)] = kv[i+1]
			}
			return m
		},
		"pct": func(part, whole int64) string {
			if whole <= 0 {
				return "0"
			}
			return fmt.Sprintf("%.1f", 100*float64(max(part, 0))/float64(whole))
		},
	}
	base := template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/parts.html"))
	u.pages = map[string]*template.Template{}
	for _, page := range []string{"library", "running", "get", "details", "signin", "forbidden", "import", "backends"} {
		t := template.Must(base.Clone())
		u.pages[page] = template.Must(t.ParseFS(templateFS, "templates/"+page+".html"))
	}
	m := http.NewServeMux()
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/app/models", http.StatusFound) })
	m.HandleFunc("GET /app", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/app/models", http.StatusFound) })
	m.HandleFunc("GET /app/assets/{name...}", u.asset)
	m.HandleFunc("GET /app/models", u.need("models:read", u.library))
	m.HandleFunc("GET /app/models/get", u.need("models:read", u.getModels))
	m.HandleFunc("GET /app/models/running", u.need("models:read", u.running))
	m.HandleFunc("GET /app/models/m/{id...}", u.need("models:read", u.details))
	m.HandleFunc("GET /app/models/import", u.need("models:manage", u.importPage))
	m.HandleFunc("POST /app/models/import/resolve", u.mutating("models:manage", u.importResolve))
	m.HandleFunc("POST /app/models/import/start", u.mutating("models:manage", u.importStart))
	m.HandleFunc("GET /app/models/import/jobs/{job}", u.need("models:manage", u.importJob))
	m.HandleFunc("GET /app/models/backend/{id}", u.need("models:read", u.backendPanel))
	m.HandleFunc("POST /app/models/backend/{id}", u.mutating("models:manage", u.backendPanel))
	m.HandleFunc("GET /app/backends", u.need("models:read", u.backendsPage))
	m.HandleFunc("POST /app/backends", u.mutating("models:manage", u.backendsPage))
	m.HandleFunc("/app/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	u.mux = m
	return u
}

func (u *UI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob:; media-src 'self' blob:; style-src 'self'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	u.mux.ServeHTTP(w, r)
}

func (u *UI) asset(w http.ResponseWriter, r *http.Request) {
	name := path.Clean(r.PathValue("name"))
	b, err := staticFS.ReadFile("static/" + name)
	if err != nil || strings.HasPrefix(name, "..") {
		http.NotFound(w, r)
		return
	}
	switch path.Ext(name) {
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	if r.URL.Query().Get("v") == u.assetVer[name] {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.Write(b)
}

// --- sign-in ------------------------------------------------------------------
//
// Pages use the browser identity the gateway resolves (Cloudflare Access or a
// LocalBase session from OIDC or a magic link). The gateway owns /app/login,
// /app/logout and /app/session.

func (u *UI) principal(r *http.Request) *auth.Principal {
	if u.d.NoAuth {
		return &auth.Principal{Kind: "browser-session", ID: "no-auth", Permissions: auth.Permissions}
	}
	if u.d.Principal != nil {
		if p := u.d.Principal(r); p != nil {
			return p
		}
	}
	return auth.Anonymous
}

type ctxKey struct{}

// need requires a permission. Without a session, htmx requests get
// HX-Redirect to sign-in and full pages a 303; without browser access
// configured, a page explains how to set it up.
func (u *UI) need(permission string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := u.principal(r)
		if !p.Authenticated() {
			if !u.d.SignInEnabled {
				w.WriteHeader(http.StatusUnauthorized)
				u.render(w, r, "signin", map[string]any{})
				return
			}
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", "/app/login")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/app/login", http.StatusSeeOther)
			return
		}
		if !p.Can(permission) {
			w.WriteHeader(http.StatusForbidden)
			u.render(w, r, "forbidden", map[string]any{"Permission": permission})
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	}
}

func (u *UI) render(w http.ResponseWriter, r *http.Request, page string, data map[string]any) {
	t := u.pages[page]
	name := "layout"
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Boosted") != "true" {
		if frag := r.Header.Get("HX-Target"); frag != "" && t.Lookup(frag) != nil {
			name = frag
		}
	}
	if p, ok := r.Context().Value(ctxKey{}).(*auth.Principal); ok {
		data["Principal"] = p
	}
	data["Page"] = page
	data["LogoutURL"] = u.d.LogoutURL
	data["NoAuth"] = u.d.NoAuth
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Vary", "HX-Request, HX-Target")
	if err := t.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}
