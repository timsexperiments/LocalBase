// Package web is the server-rendered browser UI: html/template pages and
// fragments, htmx for interaction, Tailwind CSS compiled at build time.
// Handlers call the gateway's services directly; there is no JSON client
// and no /app/api mirror.
package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
	"sync"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/library"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/voices"
)

//go:generate ../../tools/tailwind.sh

//go:embed css/theme.css
var themeCSS []byte

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
	// Call runs an API route in-process as the signed-in person (Lab, voices).
	Call func(ctx context.Context, p *auth.Principal, method, path, contentType string, body []byte) APIResult
	// Voices lists stored custom voices.
	Voices func(ctx context.Context) ([]voices.Voice, error)
}

type UI struct {
	d        Deps
	pages    map[string]*template.Template
	assetVer map[string]string
	zipMu    sync.Mutex
	zips     map[string][]byte
	mux      *http.ServeMux

	pendingMu sync.Mutex
	pending   map[string]pendingImport

	stripMemory memoryCache

	results   resultStore
	notes     labNotes
	previewMu sync.Mutex
	previews  map[string][]byte // voice samples, by model and voice
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
		"gaugeSegments": gaugeSegments,
		"themeColor":    themeColor,
		"asset":         func(name string) string { return "/app/assets/" + name + "?v=" + u.assetVer[name] },
		"gb":            func(b int64) string { return fmt.Sprintf("%.1f GB", float64(b)/1e9) },
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
		"gib":     gibText,
		"seconds": func(ms int64) float64 { return float64(ms) / 1000 },
		"list":    func(v ...string) []string { return v },
		"dict": func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				m[kv[i].(string)] = kv[i+1]
			}
			return m
		},
		"section":  section,
		"lower":    strings.ToLower,
		"canAdmin": func(p *auth.Principal) bool { return p.Can("access:read") || p.Can("keys:read") },
		"initial":  initial,
		"pct": func(part, whole int64) string {
			if whole <= 0 {
				return "0"
			}
			return fmt.Sprintf("%.1f", 100*float64(max(part, 0))/float64(whole))
		},
	}
	base := template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/parts.html"))
	u.pages = map[string]*template.Template{}
	for _, page := range []string{"library", "running", "get", "details", "signin", "forbidden", "import", "backends", "lab", "studio", "voices", "chat", "settings", "admin"} {
		t := template.Must(base.Clone())
		u.pages[page] = template.Must(t.ParseFS(templateFS, "templates/"+page+".html"))
	}
	m := http.NewServeMux()
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/app/chat", http.StatusFound) })
	m.HandleFunc("GET /app", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/app/chat", http.StatusFound) })
	m.HandleFunc("GET /app/chat", u.need("models:read", u.chat))
	m.HandleFunc("GET /app/chat/default", u.need("models:read", u.chatDefault))
	m.HandleFunc("GET /app/settings", u.need("models:read", u.settings))
	m.HandleFunc("POST /app/settings/hf-token", u.mutating("models:manage", u.hfToken))
	m.HandleFunc("POST /app/settings/default-models", u.mutating("models:manage", u.defaultModels))
	m.HandleFunc("GET /app/admin", u.admin)
	m.HandleFunc("POST /app/admin/access", u.mutating("access:manage", u.adminAccess))
	m.HandleFunc("POST /app/admin/keys", u.mutating("keys:manage", u.adminKeys))
	m.HandleFunc("GET /app/assets/{name...}", u.asset)
	m.HandleFunc("GET /app/models", u.need("models:read", u.library))
	m.HandleFunc("GET /app/models/get", u.need("models:read", u.getModels))
	m.HandleFunc("POST /app/models/get/preview", u.mutating("models:manage", u.discoverPreview))
	m.HandleFunc("POST /app/models/get/add", u.mutating("models:manage", u.discoverAdd))
	m.HandleFunc("POST /app/models/get/recipe", u.mutating("models:manage", u.discoverRecipe))
	m.HandleFunc("GET /app/models/running", u.need("models:read", u.running))
	m.HandleFunc("GET /app/strip", u.need("models:read", u.stripFragment))
	m.HandleFunc("POST /app/models/unload/{id...}", u.mutating("models:manage", u.unload))
	m.HandleFunc("GET /app/models/m/{id...}", u.need("models:read", u.details))
	m.HandleFunc("POST /app/models/placement/{id...}", u.mutating("models:manage", u.placementPost))
	m.HandleFunc("GET /app/models/import", u.need("models:manage", u.importPage))
	m.HandleFunc("POST /app/models/import/resolve", u.mutating("models:manage", u.importResolve))
	m.HandleFunc("POST /app/models/import/start", u.mutating("models:manage", u.importStart))
	m.HandleFunc("GET /app/models/import/jobs/{job}", u.need("models:manage", u.importJob))
	m.HandleFunc("GET /app/models/backend/{id}", u.need("models:read", u.backendPanel))
	m.HandleFunc("POST /app/models/backend/{id}", u.mutating("models:manage", u.backendPanel))
	m.HandleFunc("GET /app/backends", u.need("models:read", u.backendsPage))
	m.HandleFunc("POST /app/backends", u.mutating("models:manage", u.backendsPage))
	m.HandleFunc("GET /app/studio", u.need("models:read", u.studio))
	m.HandleFunc("GET /app/studio/{tool}", u.need("models:read", u.studioTool))
	m.HandleFunc("GET /app/lab", u.need("models:read", u.labRedirect))
	m.HandleFunc("GET /app/lab/voices", u.need("models:read", u.labVoiceOptions))
	m.HandleFunc("POST /app/lab/image", u.mutating("inference:image", u.labImage))
	m.HandleFunc("POST /app/lab/speech", u.mutating("inference:speech", u.labSpeech))
	m.HandleFunc("POST /app/lab/transcribe", u.uploading("inference:transcription", 26<<20, u.labTranscribe))
	m.HandleFunc("GET /app/lab/results/{id}", u.need("models:read", u.labResult))
	m.HandleFunc("GET /app/lab/runs/{id}", u.need("models:read", u.labRunGet))
	m.HandleFunc("GET /app/lab/runs/{id}/export", u.need("models:read", u.labTranscriptExport))
	m.HandleFunc("POST /app/lab/runs/{id}/stop", u.mutating("models:read", u.labRunStop))
	m.HandleFunc("POST /app/lab/runs/{id}/again", u.mutating("inference:image", u.labImageAgain))
	m.HandleFunc("POST /app/lab/runs/{id}/remove", u.mutating("models:read", u.labRunRemove))
	m.HandleFunc("GET /app/lab/video/options", u.need("models:read", u.labVideoOptions))
	m.HandleFunc("GET /app/lab/active", u.need("models:read", u.labActive))
	m.HandleFunc("GET /app/lab/voices/preview", u.need("inference:speech", u.labVoicePreview))
	m.HandleFunc("GET /app/lab/speech/{id}", u.need("models:read", u.labSpeechStatus))
	m.HandleFunc("POST /app/lab/speech/{id}/stop", u.mutating("inference:speech", u.labSpeechStop))
	m.HandleFunc("POST /app/lab/speech/{id}/again", u.mutating("inference:speech", u.labSpeechAgain))
	m.HandleFunc("POST /app/lab/speech/{id}/remove", u.mutating("inference:speech", u.labSpeechRemove))
	m.HandleFunc("POST /app/lab/video/{id}/again", u.mutating("inference:video", u.labVideoAgain))
	m.HandleFunc("POST /app/lab/video/{id}/remove", u.mutating("inference:video", u.labVideoRemove))
	m.HandleFunc("POST /app/lab/video", u.uploading("inference:video", videoFormLimit+(64<<10), u.labVideo))
	m.HandleFunc("GET /app/lab/video/{id}", u.need("inference:video", u.labVideoStatus))
	m.HandleFunc("POST /app/lab/video/{id}/cancel", u.mutating("inference:video", u.labVideoCancel))
	m.HandleFunc("GET /app/lab/video/{id}/content", u.need("inference:video", u.labVideoContent))
	m.HandleFunc("GET /app/voices", u.need("models:read", u.voicesPage))
	m.HandleFunc("POST /app/voices", u.uploading("models:manage", 13<<20, u.voiceCreate))
	m.HandleFunc("POST /app/voices/{id}/rename", u.mutating("models:manage", u.voiceRename))
	m.HandleFunc("POST /app/voices/{id}/delete", u.mutating("models:manage", u.voiceDelete))
	m.HandleFunc("GET /app/voices/{id}/audio", u.need("models:manage", u.voiceAudio))
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
	case ".svg":
		w.Header().Set("Content-Type", "image/svg+xml")
	case ".png":
		w.Header().Set("Content-Type", "image/png")
	case ".woff2":
		w.Header().Set("Content-Type", "font/woff2")
	case ".webmanifest":
		w.Header().Set("Content-Type", "application/manifest+json")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	// A font's name carries its version.
	if r.URL.Query().Get("v") == u.assetVer[name] || strings.HasPrefix(name, "fonts/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	if t := w.Header().Get("Content-Type"); strings.HasPrefix(t, "text/") || t == "image/svg+xml" {
		w.Header().Set("Vary", "Accept-Encoding")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			b = u.zipped(name, b)
		}
	}
	w.Write(b)
}

// themeColor reads the browser bar's color from the theme file, so a direction is one file.
func themeColor(scheme string) string {
	m := regexp.MustCompile(`theme-color ` + regexp.QuoteMeta(scheme) + `: (#[0-9a-fA-F]{6})`).FindSubmatch(themeCSS)
	if m == nil {
		return ""
	}
	return string(m[1])
}

// zipped compresses an embedded asset once.
func (u *UI) zipped(name string, b []byte) []byte {
	u.zipMu.Lock()
	defer u.zipMu.Unlock()
	if z, ok := u.zips[name]; ok {
		return z
	}
	var out bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&out, gzip.BestCompression)
	zw.Write(b)
	zw.Close()
	if u.zips == nil {
		u.zips = map[string][]byte{}
	}
	u.zips[name] = out.Bytes()
	return out.Bytes()
}

// section is the top-level destination a page belongs to: it marks the
// current tab on phones and the current item in the desktop sidebar.
func section(page string) string {
	switch page {
	case "chat", "running":
		return page
	case "studio", "lab":
		return "studio"
	case "library", "get", "details", "import", "backends":
		return "models"
	}
	return "more" // voices, settings, admin
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
	// Everywhere someone is signed in. Running draws its own, with the legend under it.
	if name == "layout" && data["Principal"] != nil && page != "running" {
		if strip := u.memoryStrip(r); strip != nil {
			data["Strip"] = strip
		}
	}
	data["CSRF"] = u.csrfToken(w, r)
	data["LogoutURL"] = u.d.LogoutURL
	data["NoAuth"] = u.d.NoAuth
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Vary", "HX-Request, HX-Target")
	if err := t.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

// initial is a name's first letter, upper-cased, for avatars ("A" if none).
func initial(name string) string {
	for _, r := range strings.TrimSpace(name) {
		return strings.ToUpper(string(r))
	}
	return "A"
}
