// Package web is the server-rendered browser UI: html/template pages and
// fragments, htmx for interaction, Tailwind CSS compiled at build time.
// Handlers call the gateway's services directly; there is no JSON client
// and no /app/api mirror.
package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
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
	Store    *store.Store
	Catalog  *catalog.Catalog
	LLM      *runtime.LLM
	Auth     *auth.Resolver
	Memory   *memory.Controller
	NoAuth   bool
	DevLogin bool
	RootDir  string
}

type UI struct {
	d        Deps
	pages    map[string]*template.Template
	assetVer map[string]string
	mux      *http.ServeMux

	mu       sync.Mutex
	sessions map[string]session
}

type session struct {
	p       *auth.Principal
	expires time.Time
}

const (
	sessionCookie = "lb_session"
	sessionIdle   = 12 * time.Hour
)

func New(d Deps) *UI {
	u := &UI{d: d, sessions: map[string]session{}, assetVer: map[string]string{}}
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
	for _, page := range []string{"library", "running", "get", "details", "login"} {
		t := template.Must(base.Clone())
		u.pages[page] = template.Must(t.ParseFS(templateFS, "templates/"+page+".html"))
	}
	m := http.NewServeMux()
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/app/models", http.StatusFound) })
	m.HandleFunc("GET /app", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/app/models", http.StatusFound) })
	m.HandleFunc("GET /app/assets/{name...}", u.asset)
	m.HandleFunc("GET /app/login", u.loginPage)
	m.HandleFunc("POST /app/login", u.login)
	m.HandleFunc("POST /app/logout", u.logout)
	m.HandleFunc("GET /app/session", u.sessionProbe)
	m.HandleFunc("GET /app/models", u.need("models:read", u.library))
	m.HandleFunc("GET /app/models/get", u.need("models:read", u.getModels))
	m.HandleFunc("GET /app/models/running", u.need("models:read", u.running))
	m.HandleFunc("GET /app/models/m/{id...}", u.need("models:read", u.details))
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

// --- sessions ---------------------------------------------------------------
//
// Milestone 0 ships only a development sign-in (an API key exchanged for an
// opaque, in-memory session cookie). OIDC and Cloudflare Access replace it in
// milestone 3 behind the same principal type.

func (u *UI) principal(r *http.Request) *auth.Principal {
	if u.d.NoAuth {
		return &auth.Principal{Kind: "browser-session", ID: "no-auth", Permissions: auth.Permissions}
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return auth.Anonymous
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	s, ok := u.sessions[c.Value]
	if !ok || time.Now().After(s.expires) {
		delete(u.sessions, c.Value)
		return auth.Anonymous
	}
	s.expires = time.Now().Add(sessionIdle)
	u.sessions[c.Value] = s
	return s.p
}

type ctxKey struct{}

// need requires a permission. An expired session sends htmx requests to
// sign-in with HX-Redirect and full pages with a 303, keeping the return URL.
func (u *UI) need(permission string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := u.principal(r)
		if !p.Authenticated() {
			target := r.URL.RequestURI()
			if cur := r.Header.Get("HX-Current-URL"); cur != "" {
				if cu, err := url.Parse(cur); err == nil {
					target = cu.RequestURI()
				}
			}
			login := "/app/login?next=" + url.QueryEscape(target)
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", login)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, login, http.StatusSeeOther)
			return
		}
		if !p.Can(permission) {
			http.Error(w, "This account cannot view models.", http.StatusForbidden)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	}
}

func safeNext(next string) string {
	if !strings.HasPrefix(next, "/app") || strings.HasPrefix(next, "//") {
		return "/app/models"
	}
	return next
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return r.Header.Get("Sec-Fetch-Site") == "same-origin"
	}
	o, err := url.Parse(origin)
	return err == nil && o.Host == r.Host
}

func (u *UI) loginPage(w http.ResponseWriter, r *http.Request) {
	u.render(w, r, "login", map[string]any{"Next": safeNext(r.URL.Query().Get("next")), "DevLogin": u.d.DevLogin})
}

func (u *UI) login(w http.ResponseWriter, r *http.Request) {
	if !u.d.DevLogin || !sameOrigin(r) {
		http.Error(w, "Sign-in is not available.", http.StatusForbidden)
		return
	}
	r.ParseForm()
	next := safeNext(r.PostForm.Get("next"))
	p, err := u.d.Auth.ResolveKey(r.Context(), strings.TrimSpace(r.PostForm.Get("key")))
	if err != nil || !p.Authenticated() || !p.Can("models:read") {
		w.WriteHeader(http.StatusUnauthorized)
		u.render(w, r, "login", map[string]any{"Next": next, "DevLogin": true, "Error": "That key was not accepted. It needs the models:read permission."})
		return
	}
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	u.mu.Lock()
	u.sessions[token] = session{p: &auth.Principal{Kind: "browser-session", ID: "dev:" + p.ID, Name: p.Name, Permissions: p.Permissions}, expires: time.Now().Add(sessionIdle)}
	u.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (u *UI) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && sameOrigin(r) {
		u.mu.Lock()
		delete(u.sessions, c.Value)
		u.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true})
	http.Redirect(w, r, "/app/login", http.StatusSeeOther)
}

// sessionProbe lets the page tell a LocalBase sign-in expiry (401 here)
// from an edge sign-in expiry (the edge redirects this fetch) (#363/#364).
func (u *UI) sessionProbe(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !u.principal(r).Authenticated() {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"authenticated":false}`))
		return
	}
	w.Write([]byte(`{"authenticated":true}`))
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
	data["DevLogin"] = u.d.DevLogin
	data["NoAuth"] = u.d.NoAuth
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Vary", "HX-Request, HX-Target")
	if err := t.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}
