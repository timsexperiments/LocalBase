package gateway

import (
	"context"
	"net/http"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

// Browser access routes (ui/access.ts): sign-in, sign-out, the session probe,
// and /app/api/* forwarding, which lets the UI's own fetches (chat streaming,
// media, admin) reach the API as the signed-in browser identity.

// IsUIAccessPath reports the paths the browser layer owns.
func IsUIAccessPath(p string) bool {
	switch p {
	case "/app/login", auth.OIDCCallbackPath, "/github/callback", auth.MagicCallbackPath, auth.MagicRequestPath,
		"/app/logout", "/app/session", "/app/api":
		return true
	}
	return strings.HasPrefix(p, "/app/api/")
}

// allowedUIRoute lists what the browser may call through /app/api.
func allowedUIRoute(method, p string) bool {
	switch p {
	case "/_localbase/access-management", "/_localbase/api-keys", "/_localbase/model-management", "/_localbase/hf-token":
		return method == "GET" || method == "POST"
	case "/_localbase/model-backend", "/_localbase/model-imports":
		return method == "GET" || method == "POST"
	case "/_localbase/backends", "/_localbase/auth-profiles":
		return method == "GET"
	case "/_localbase/model-discovery":
		return method == "POST"
	case "/_localbase/models":
		return method == "GET"
	case "/_localbase/voices":
		return method == "GET" || method == "POST"
	case "/v1/chat/completions", "/v1/embeddings", "/v1/systemone", "/v1/images/generations", "/v1/images/edits",
		"/v1/audio/speech", "/v1/audio/transcriptions", "/v1/videos":
		return method == "POST"
	}
	if rest, ok := strings.CutPrefix(p, "/_localbase/voices/"); ok && rest != "" {
		if strings.HasSuffix(rest, "/audio") && strings.Count(rest, "/") == 1 {
			return method == "GET"
		}
		if !strings.Contains(rest, "/") {
			return method == "GET" || method == "PATCH" || method == "DELETE"
		}
		return false
	}
	if rest, ok := strings.CutPrefix(p, "/v1/videos/"); ok && rest != "" {
		switch {
		case strings.HasSuffix(rest, "/content"):
			return method == "GET"
		case strings.HasSuffix(rest, "/cancel"):
			return method == "POST"
		case !strings.Contains(rest, "/"):
			return method == "GET" || method == "DELETE"
		}
	}
	return false
}

// BrowserPrincipal resolves the signed-in browser identity of a request.
func (s *Server) BrowserPrincipal(r *http.Request) (*auth.Principal, *auth.Identity) {
	if !s.Browser.Enabled() {
		return nil, nil
	}
	id, owner := s.Browser.Authenticate(r)
	if id == nil {
		return nil, nil
	}
	d := s.Browser.Authorize(r.Context(), id)
	return &auth.Principal{Kind: "browser-session", ID: owner, Name: id.VerifiedEmail, Permissions: d.Permissions, Identity: id}, id
}

type browserPrincipalKey struct{}

// uiAccess serves the browser-owned paths.
func (s *Server) uiAccess(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	b := s.Browser
	if s.NoAuth && (p == "/app/session" || strings.HasPrefix(p, "/app/api/")) {
		// --no-auth (loopback development): the UI acts with every permission.
		full := &auth.Principal{Kind: "browser-session", ID: "no-auth", Permissions: auth.Permissions}
		if p == "/app/session" {
			writeJSON(w, 200, map[string]any{"authenticated": true, "permissions": full.Permissions})
			return
		}
		target := strings.TrimPrefix(p, "/app/api")
		if !allowedUIRoute(r.Method, target) {
			auth.Deny(w, 404)
			return
		}
		fwd := r.Clone(context.WithValue(r.Context(), browserPrincipalKey{}, full))
		fwd.URL.Path, fwd.URL.RawPath, fwd.RequestURI = target, "", target
		s.mux.ServeHTTP(w, fwd)
		return
	}
	if !b.Enabled() {
		auth.Deny(w, 401)
		return
	}
	exact := b.ExactHost(r)
	switch p {
	case "/app/login":
		switch {
		case r.Method != http.MethodGet:
			auth.Deny(w, 405)
		case !exact:
			auth.Deny(w, 403)
		default:
			b.StartLogin(w, r)
		}
		return
	case auth.OIDCCallbackPath:
		if r.Method != http.MethodGet || !exact || !b.Direct() {
			auth.Deny(w, 403)
			return
		}
		b.CompleteLogin(w, r)
		return
	case "/github/callback":
		auth.Deny(w, 403) // GitHub OAuth is not offered (DESIGN §11)
		return
	case auth.MagicRequestPath:
		if r.Method != http.MethodPost || !exact || !b.Direct() {
			auth.Deny(w, 403)
			return
		}
		b.RequestMagicLink(w, r, func(to, link string) error {
			cfg, err := auth.LoadEmail(s.Root.Dir)
			if err != nil || cfg == nil {
				return err
			}
			return auth.SendEmail(cfg, to, "Sign in to LocalBase",
				"Sign in to LocalBase:\n\n"+link+"\n\nThis link expires in 15 minutes and can be used once.")
		})
		return
	case auth.MagicCallbackPath:
		switch {
		case !exact || !b.Direct():
			auth.Deny(w, 403)
		case r.Method == http.MethodGet:
			b.MagicLinkPage(w)
		case r.Method == http.MethodPost:
			b.CompleteMagicLink(w, r)
		default:
			auth.Deny(w, 403)
		}
		return
	case "/app/logout":
		if r.Method != http.MethodPost || !b.SameOrigin(r) {
			auth.Deny(w, 403)
			return
		}
		b.Logout(w, r)
		return
	}
	session := p == "/app/session"
	if session && r.Method != http.MethodGet {
		auth.Deny(w, 405)
		return
	}
	if !b.SameOrigin(r) {
		auth.Deny(w, 403)
		return
	}
	target := strings.TrimPrefix(p, "/app/api")
	if !session && !allowedUIRoute(r.Method, target) {
		auth.Deny(w, 404)
		return
	}
	principal, id := s.BrowserPrincipal(r)
	if principal == nil {
		auth.Deny(w, 401)
		return
	}
	setAuth(r, principal)
	if session {
		body := map[string]any{"authenticated": true, "permissions": principal.Permissions}
		if id.VerifiedEmail != "" {
			body["verifiedEmail"] = id.VerifiedEmail
		}
		if out := b.CloudflareLogout(); out != "" {
			body["logoutUrl"] = out
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, body)
		return
	}
	// Forward to the API route as the browser identity, without credentials.
	fwd := r.Clone(context.WithValue(r.Context(), browserPrincipalKey{}, principal))
	for name := range fwd.Header {
		low := strings.ToLower(name)
		if strings.HasPrefix(low, "cf-access-") || low == "cookie" || low == "authorization" || low == "x-api-key" || low == "x-localbase-ui" {
			fwd.Header.Del(name)
		}
	}
	fwd.URL.Path, fwd.URL.RawPath, fwd.URL.RawQuery, fwd.RequestURI = target, "", "", target
	s.mux.ServeHTTP(w, fwd)
}
