package gateway

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
)

// Browser access routes (ui/access.ts): sign-in, sign-out, the session probe,
// and /app/api/* forwarding, which lets the UI's own fetches (chat streaming,
// media, admin) reach the API as the signed-in browser identity.

// IsUIAccessPath reports the paths the browser layer owns.
func IsUIAccessPath(p string) bool {
	switch p {
	case "/app/login", auth.LocalOncePath, auth.OIDCCallbackPath, "/github/callback", auth.MagicCallbackPath, auth.MagicRequestPath,
		"/app/logout", "/app/session", "/app/api":
		return true
	}
	return strings.HasPrefix(p, "/app/api/")
}

// allowedUIRoute lists what the browser may call through /app/api: the route table's ui methods
// for an exact path, or its uiSub rule for a path under a pattern (ui_oracle_test.go keeps the list
// as it was written by hand, and checks this against it).
func allowedUIRoute(method, p string) bool {
	ix := tableIndex()
	if methods, ok := ix.uiExact[p]; ok {
		return slices.Contains(methods, method)
	}
	for _, sub := range ix.uiPrefixed {
		if rest, ok := strings.CutPrefix(p, sub.prefix); ok {
			return sub.allow(rest, method)
		}
	}
	return false
}

// uiVoice is what the browser may call under /_localbase/voices/.
func uiVoice(rest, method string) bool {
	if rest == "" {
		return false
	}
	if strings.HasSuffix(rest, "/audio") && strings.Count(rest, "/") == 1 {
		return method == "GET"
	}
	if !strings.Contains(rest, "/") {
		return method == "GET" || method == "PATCH" || method == "DELETE"
	}
	return false
}

// uiModel is what the browser may call under /_localbase/models/.
func uiModel(rest, method string) bool {
	if id, ok := strings.CutSuffix(rest, "/unload"); ok && id != "" && !strings.Contains(id, "/") {
		return method == "POST"
	}
	if id, ok := strings.CutSuffix(rest, "/admission"); ok && id != "" && !strings.Contains(id, "/") {
		return method == "GET"
	}
	return false
}

// uiRun is what the browser may call under /_localbase/runs/.
func uiRun(rest, method string) bool {
	if rest == "" {
		return false
	}
	if target, ok := strings.CutPrefix(rest, "v1/"); ok {
		_, known := runTargets["/v1/"+target]
		return known && method == "POST"
	}
	id, action, _ := strings.Cut(rest, "/")
	switch {
	case id == "":
		return false
	case action == "":
		return method == "GET" || method == "DELETE"
	case action == "events":
		return method == "GET"
	case action == "cancel":
		return method == "POST"
	case action == "result":
		return method == "GET"
	}
	return false
}

// uiVideoJob is what the browser may call under /v1/videos/.
func uiVideoJob(rest, method string) bool {
	if rest == "" {
		return false
	}
	switch {
	case strings.HasSuffix(rest, "/content"):
		return method == "GET"
	case strings.HasSuffix(rest, "/cancel"):
		return method == "POST"
	case !strings.Contains(rest, "/"):
		return method == "GET" || method == "DELETE"
	}
	return false
}

// BrowserPrincipal resolves the signed-in browser identity of a request.
func (s *Server) BrowserPrincipal(r *http.Request) (*auth.Principal, *auth.Identity) {
	if p := s.localPrincipal(r); p != nil {
		return p, nil
	}
	if !s.Browser.Enabled() {
		return nil, nil
	}
	id, owner, err := s.Browser.AuthenticateChecked(r)
	if err != nil {
		s.lookupFailed(r, err)
		return nil, nil
	}
	if id == nil {
		return nil, nil
	}
	d, err := s.Browser.AuthorizeChecked(r.Context(), id)
	if err != nil {
		auth.MarkLookupFailed(r.Context()) // Authorize has logged it
		return nil, nil
	}
	httpx.MarkAuthenticated(r)
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
			httpx.JSON(w, 200, map[string]any{"authenticated": true, "permissions": full.Permissions})
			return
		}
		target := strings.TrimPrefix(p, "/app/api")
		if !allowedUIRoute(r.Method, target) {
			auth.Deny(w, 404)
			return
		}
		fwd := r.Clone(context.WithValue(r.Context(), browserPrincipalKey{}, full))
		fwd.URL.Path, fwd.URL.RawPath, fwd.RequestURI = target, "", target
		fwd.URL.RawQuery = forwardedQuery(target, r)
		s.mux.ServeHTTP(w, fwd)
		r.Pattern = fwd.Pattern // the route it reached: usage, the log and the span read it from r
		return
	}
	if s.localUIAccess(w, r) || httpx.CredentialLookupFailed(w, r) {
		return
	}
	if !b.Enabled() {
		// Only the pages people open: the session and API paths stay as they are.
		if p != "/app/session" && !strings.HasPrefix(p, "/app/api") {
			s.loginHelp(w, r, 401)
			return
		}
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
			s.denyPerson(w, r, 403)
		default:
			b.StartLogin(w, r)
		}
		return
	case auth.OIDCCallbackPath:
		if r.Method != http.MethodGet || !exact || !b.Direct() {
			s.denyPerson(w, r, 403)
			return
		}
		b.CompleteLogin(w, r)
		return
	case "/github/callback":
		s.denyPerson(w, r, 403) // GitHub OAuth is not offered (DESIGN §11)
		return
	case auth.MagicRequestPath:
		if r.Method != http.MethodPost || !exact || !b.Direct() {
			s.denyPerson(w, r, 403)
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
			s.denyPerson(w, r, 403)
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
			s.denyPerson(w, r, 403)
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
	target := strings.TrimPrefix(p, "/app/api")
	if !b.SameOrigin(r) && !(isRunEvents(target) && b.SameOriginStream(r)) {
		auth.Deny(w, 403)
		return
	}
	if !session && !allowedUIRoute(r.Method, target) {
		auth.Deny(w, 404)
		return
	}
	principal, id := s.BrowserPrincipal(r)
	if principal == nil {
		if !httpx.CredentialLookupFailed(w, r) {
			auth.Deny(w, 401)
		}
		return
	}
	httpx.SetAuth(r, principal)
	if session {
		body := map[string]any{"authenticated": true, "permissions": principal.Permissions}
		if id.VerifiedEmail != "" {
			body["verifiedEmail"] = id.VerifiedEmail
		}
		if out := b.CloudflareLogout(); out != "" {
			body["logoutUrl"] = out
		}
		w.Header().Set("Cache-Control", "no-store")
		httpx.JSON(w, 200, body)
		return
	}
	s.forwardAsBrowser(w, r, principal, target)
}

// forwardAsBrowser runs an API route as the browser identity, without credentials.
func (s *Server) forwardAsBrowser(w http.ResponseWriter, r *http.Request, principal *auth.Principal, target string) {
	fwd := r.Clone(context.WithValue(r.Context(), browserPrincipalKey{}, principal))
	for name := range fwd.Header {
		low := strings.ToLower(name)
		if strings.HasPrefix(low, "cf-access-") || low == "cookie" || low == "authorization" || low == "x-api-key" || low == "x-localbase-ui" {
			fwd.Header.Del(name)
		}
	}
	fwd.URL.Path, fwd.URL.RawPath, fwd.URL.RawQuery, fwd.RequestURI = target, "", "", target
	fwd.URL.RawQuery = forwardedQuery(target, r)
	s.mux.ServeHTTP(w, fwd)
	r.Pattern = fwd.Pattern // the route it reached: usage, the log and the span read it from r
}

// isRunEvents reports a run's event stream path (/_localbase/runs/{id}/events).
func isRunEvents(target string) bool {
	rest, ok := strings.CutPrefix(target, "/_localbase/runs/")
	id, action, _ := strings.Cut(rest, "/")
	return ok && id != "" && !strings.HasPrefix(rest, "v1/") && action == "events"
}

// forwardedQuery keeps the query parameters a route reads: `after` on a run's
// event stream, `wait` on a model unload, and usage's filters. Forwarding drops
// every other query.
func forwardedQuery(target string, r *http.Request) string {
	if target == "/_localbase/usage" {
		kept := url.Values{}
		for _, name := range []string{"range", "from", "to", "scope", "group", "principal", "model", "route"} {
			if v, ok := r.URL.Query()[name]; ok {
				kept[name] = v
			}
		}
		return kept.Encode()
	}
	key := ""
	switch {
	case isRunEvents(target):
		key = "after"
	case strings.HasPrefix(target, "/_localbase/models/") && strings.HasSuffix(target, "/unload"):
		key = "wait"
	case strings.HasPrefix(target, "/_localbase/models/") && strings.HasSuffix(target, "/admission"):
		key = "assumeInstalled"
	}
	if v := r.URL.Query().Get(key); key != "" && v != "" {
		return url.Values{key: {v}}.Encode()
	}
	return ""
}
