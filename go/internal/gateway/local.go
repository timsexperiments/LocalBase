package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

// LoginPages renders the browser pages of local sign-in (DESIGN §28). The UI
// provides them; the status is already chosen here.
type LoginPages interface {
	LoginHelp(w http.ResponseWriter, r *http.Request, status int)
	LoginOnce(w http.ResponseWriter, r *http.Request)
	LoginExpired(w http.ResponseWriter, r *http.Request, status int)
	LoginTooMany(w http.ResponseWriter, r *http.Request)
	// LoginWrongAddress says where sign-in lives, for a request to another host name.
	LoginWrongAddress(w http.ResponseWriter, r *http.Request, origin string)
	// LoginUnavailable is for a person who followed a way in this gateway doesn't offer.
	LoginUnavailable(w http.ResponseWriter, r *http.Request, status int)
}

// localLimit counts failed redeems: at most 10 a minute per gateway.
type localLimit struct {
	mu    sync.Mutex
	start time.Time
	count int
}

const localRedeemFailures = 10

func (l *localLimit) blocked(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.start) >= time.Minute {
		l.start, l.count = now, 0
	}
	return l.count >= localRedeemFailures
}

func (l *localLimit) fail() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.count++
}

// localPrincipal is the request's local session, if it has one and the request
// passes the loopback rule. Every permission is intentional (auth/local.go). The
// nil Identity keeps it out of the lockout rules' count of managers.
func (s *Server) localPrincipal(r *http.Request) *auth.Principal {
	token := auth.LocalCookieToken(r)
	if token == "" || !auth.LocalRequest(r) {
		return nil
	}
	sess, err := auth.FindLocalSession(r.Context(), s.Store, token)
	if err != nil || sess == nil {
		return nil
	}
	return &auth.Principal{Kind: "browser-session", ID: auth.LocalOwnerID, Permissions: auth.Permissions}
}

// localNavigation: a browser opening a page, as against an API client.
func localNavigation(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

func (s *Server) localRefused(r *http.Request, reason string) {
	s.Log.Warn("Local sign-in was refused.", "event", "auth.local-login-refused", "reason", reason)
}

// localOnce serves GET and POST /app/login/once. GET only shows the page, so a
// link preview or prefetch cannot use a link up.
func (s *Server) localOnce(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		auth.Deny(w, 405)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !auth.LocalRequest(r) {
		s.localRefused(r, "not-loopback")
		s.loginHelp(w, r, 403)
		return
	}
	if r.Method == http.MethodGet {
		if s.LoginPages == nil {
			auth.Deny(w, 404)
			return
		}
		s.LoginPages.LoginOnce(w, r)
		return
	}
	if s.localFails.blocked(time.Now()) {
		s.localRefused(r, "rate-limited")
		w.Header().Set("Retry-After", "60")
		if s.LoginPages == nil {
			auth.Deny(w, 429)
			return
		}
		s.LoginPages.LoginTooMany(w, r)
		return
	}
	expired := func() {
		s.localFails.fail()
		s.localRefused(r, "expired")
		if s.LoginPages == nil {
			auth.Deny(w, 410)
			return
		}
		s.LoginPages.LoginExpired(w, r, 410)
	}
	if !auth.LocalFormOK(r) {
		expired()
		return
	}
	token, ok := auth.ReadLocalToken(r)
	if !ok {
		expired()
		return
	}
	spent, err := auth.ConsumeLocalLink(r.Context(), s.Store, token)
	if err != nil || !spent {
		expired()
		return
	}
	cookieToken, id, err := auth.CreateLocalSession(r.Context(), s.Store)
	if err != nil {
		s.Log.Error("Could not create a local session.", "event", "auth.local-session-failed", "error", err.Error())
		auth.Deny(w, 503)
		return
	}
	s.Log.Info("Local sign-in link used.", "event", "audit.local-login-redeemed", "link_id", auth.LocalLinkID(token),
		"session_id", id, "actor_kind", "local")
	auth.Redirect(w, "/app", auth.LocalSessionCookie(cookieToken))
}

// denyPerson refuses a sign-in route. A person opening a page gets words; anything else gets
// the usual answer.
func (s *Server) denyPerson(w http.ResponseWriter, r *http.Request, status int) {
	switch b := s.Browser; {
	case s.LoginPages == nil || !localNavigation(r):
		auth.Deny(w, status)
	case !b.ExactHost(r):
		s.LoginPages.LoginWrongAddress(w, r, b.Config.Origin)
	default:
		s.LoginPages.LoginUnavailable(w, r, status)
	}
}

func (s *Server) loginHelp(w http.ResponseWriter, r *http.Request, status int) {
	if s.LoginPages == nil || !localNavigation(r) {
		auth.Deny(w, status)
		return
	}
	s.LoginPages.LoginHelp(w, r, status)
}

// localUIAccess handles what a local session or local sign-in owns among the
// browser paths, and reports whether it answered.
func (s *Server) localUIAccess(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	switch {
	case p == auth.LocalOncePath:
		s.localOnce(w, r)
		return true
	case p == "/app/login" && !s.Browser.Enabled() && r.Method == http.MethodGet && localNavigation(r):
		s.loginHelp(w, r, 403)
		return true
	case p == "/app/logout":
		token := auth.LocalCookieToken(r)
		if token == "" || !auth.LocalRequest(r) {
			return false
		}
		if r.Method != http.MethodPost || !auth.LocalSameOrigin(r) {
			auth.Deny(w, 403)
			return true
		}
		auth.DeleteLocalSession(r.Context(), s.Store, token)
		http.SetCookie(w, auth.ClearLocalSessionCookie())
		w.WriteHeader(http.StatusNoContent)
		return true
	case p != "/app/session" && !strings.HasPrefix(p, "/app/api/"):
		return false
	}
	principal := s.localPrincipal(r)
	if principal == nil {
		return false
	}
	session := p == "/app/session"
	if session && r.Method != http.MethodGet {
		auth.Deny(w, 405)
		return true
	}
	target := strings.TrimPrefix(p, "/app/api")
	if !auth.LocalSameOrigin(r) && !(isRunEvents(target) && auth.LocalSameOriginStream(r)) {
		auth.Deny(w, 403)
		return true
	}
	if !session && !allowedUIRoute(r.Method, target) {
		auth.Deny(w, 404)
		return true
	}
	setAuth(r, principal)
	if session {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, map[string]any{"authenticated": true, "permissions": principal.Permissions, "localSession": true})
		return true
	}
	s.forwardAsBrowser(w, r, principal, target)
	return true
}

// localSessionsAPI lists and revokes local sessions: GET needs sessions:read and
// POST sessions:revoke. Bodies never carry a token.
func (s *Server) localSessionsAPI(w http.ResponseWriter, r *http.Request) {
	p := s.principal(r)
	if !p.Authenticated() {
		unauthorized(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		if !p.Can("sessions:read") {
			forbidden(w, r)
			return
		}
		list, err := auth.ListLocalSessions(r.Context(), s.Store)
		if err != nil {
			serviceUnavailable(w, r, "Local sessions")
			return
		}
		writeJSON(w, 200, map[string]any{"sessions": list})
	case http.MethodPost:
		if !p.Can("sessions:revoke") {
			forbidden(w, r)
			return
		}
		var in struct {
			Action string `json:"action"`
			ID     string `json:"id"`
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1025))
		if err != nil || len(body) > 1024 || json.Unmarshal(body, &in) != nil {
			badRequest(w, r, "Invalid local session payload.", nil)
			return
		}
		revoked := 0
		switch {
		case in.Action == "revoke-all" && in.ID == "":
			revoked, err = auth.RevokeAllLocal(r.Context(), s.Store)
		case in.Action == "revoke" && in.ID != "":
			var found bool
			found, err = auth.RevokeLocalSession(r.Context(), s.Store, in.ID)
			if found {
				revoked = 1
			}
		default:
			badRequest(w, r, "Invalid local session payload.", nil)
			return
		}
		if err != nil {
			serviceUnavailable(w, r, "Local sessions")
			return
		}
		s.Log.Info("Local sessions revoked.", "event", "audit.local-sessions-revoked", "actor_kind", p.Kind, "actor_id", p.ID,
			"session_id", in.ID, "count", revoked)
		writeJSON(w, 200, map[string]any{"revoked": revoked})
	default:
		methodNotAllowed(w, r, "GET, POST")
	}
}
