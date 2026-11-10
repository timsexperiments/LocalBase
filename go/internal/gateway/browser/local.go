package browser

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
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
	// LoginBusy is for when who someone is couldn't be read: not signed out, not refused.
	LoginBusy(w http.ResponseWriter, r *http.Request)
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
func (a *Access) localPrincipal(r *http.Request) *auth.Principal {
	token := auth.LocalCookieToken(r)
	if token == "" || !auth.LocalRequest(r) {
		return nil
	}
	sess, err := auth.FindLocalSession(r.Context(), a.opts.Store, token)
	if err != nil {
		a.opts.LookupFailed(r, err)
		return nil
	}
	if sess == nil {
		return nil
	}
	return &auth.Principal{Kind: "browser-session", ID: auth.LocalOwnerID, Permissions: auth.Permissions}
}

// localNavigation: a browser opening a page, as against an API client.
func localNavigation(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

func (a *Access) localRefused(r *http.Request, reason string) {
	a.opts.Log().Warn("Local sign-in was refused.", "event", "auth.local-login-refused", "reason", reason)
}

// localOnce serves GET and POST /app/login/once. GET only shows the page, so a
// link preview or prefetch cannot use a link up.
func (a *Access) localOnce(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		auth.Deny(w, 405)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !auth.LocalRequest(r) {
		a.localRefused(r, "not-loopback")
		a.loginHelp(w, r, 403)
		return
	}
	if r.Method == http.MethodGet {
		if a.opts.Pages() == nil {
			auth.Deny(w, 404)
			return
		}
		a.opts.Pages().LoginOnce(w, r)
		return
	}
	if a.localFails.blocked(time.Now()) {
		a.localRefused(r, "rate-limited")
		w.Header().Set("Retry-After", "60")
		if a.opts.Pages() == nil {
			auth.Deny(w, 429)
			return
		}
		a.opts.Pages().LoginTooMany(w, r)
		return
	}
	expired := func() {
		a.localFails.fail()
		a.localRefused(r, "expired")
		if a.opts.Pages() == nil {
			auth.Deny(w, 410)
			return
		}
		a.opts.Pages().LoginExpired(w, r, 410)
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
	spent, err := auth.ConsumeLocalLink(r.Context(), a.opts.Store, token)
	if err != nil {
		// Not known to be spent or expired, so it isn't called that, or counted as a bad try.
		a.opts.Log().Error("Could not read a local sign-in link.", "event", "auth.local-link-failed", "error", err.Error())
		w.Header().Set("Retry-After", "5")
		if a.opts.Pages() == nil {
			auth.Deny(w, http.StatusServiceUnavailable)
			return
		}
		a.opts.Pages().LoginBusy(w, r)
		return
	}
	if !spent {
		expired()
		return
	}
	cookieToken, id, err := auth.CreateLocalSession(r.Context(), a.opts.Store)
	if err != nil {
		a.opts.Log().Error("Could not create a local session.", "event", "auth.local-session-failed", "error", err.Error())
		auth.Deny(w, 503)
		return
	}
	a.opts.Log().Info("Local sign-in link used.", "event", "audit.local-login-redeemed", "link_id", auth.LocalLinkID(token),
		"session_id", id, "actor_kind", "local")
	auth.Redirect(w, "/app", auth.LocalSessionCookie(cookieToken))
}

// denyPerson refuses a sign-in route. A person opening a page gets words; anything else gets
// the usual answer.
func (a *Access) denyPerson(w http.ResponseWriter, r *http.Request, status int) {
	switch b := a.opts.Browser(); {
	case a.opts.Pages() == nil || !localNavigation(r):
		auth.Deny(w, status)
	case !b.ExactHost(r):
		a.opts.Pages().LoginWrongAddress(w, r, b.Config.Origin)
	default:
		a.opts.Pages().LoginUnavailable(w, r, status)
	}
}

func (a *Access) loginHelp(w http.ResponseWriter, r *http.Request, status int) {
	if a.opts.Pages() == nil || !localNavigation(r) {
		auth.Deny(w, status)
		return
	}
	a.opts.Pages().LoginHelp(w, r, status)
}

// localUIAccess handles what a local session or local sign-in owns among the
// browser paths, and reports whether it answered.
func (a *Access) localUIAccess(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	switch {
	case p == auth.LocalOncePath:
		a.localOnce(w, r)
		return true
	case p == "/app/login" && !a.opts.Browser().Enabled() && r.Method == http.MethodGet && localNavigation(r):
		a.loginHelp(w, r, 403)
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
		if err := auth.DeleteLocalSession(r.Context(), a.opts.Store, token); err != nil {
			a.opts.Log().Error("Could not end a local session; it stays valid until it expires.", "event", "local-session.delete-failed", "error", err.Error())
			// The session is still good, so the person isn't told otherwise and keeps the cookie.
			w.Header().Set("Retry-After", "5")
			auth.Deny(w, http.StatusServiceUnavailable)
			return true
		}
		http.SetCookie(w, auth.ClearLocalSessionCookie())
		w.WriteHeader(http.StatusNoContent)
		return true
	case p != "/app/session" && !strings.HasPrefix(p, "/app/api/"):
		return false
	}
	principal := a.localPrincipal(r)
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
	if !session && !a.opts.Allowed(r.Method, target) {
		auth.Deny(w, 404)
		return true
	}
	httpx.SetAuth(r, principal)
	if session {
		w.Header().Set("Cache-Control", "no-store")
		httpx.JSON(w, 200, map[string]any{"authenticated": true, "permissions": principal.Permissions, "localSession": true})
		return true
	}
	a.forwardAsBrowser(w, r, principal, target)
	return true
}

// LocalSessionsAPI lists and revokes local sessions: GET needs sessions:read and
// POST sessions:revoke. Bodies never carry a token.
func (a *Access) LocalSessionsAPI(w http.ResponseWriter, r *http.Request) {
	p := a.opts.Caller(r)
	if !p.Authenticated() {
		httpx.Unauthorized(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		if !p.Can("sessions:read") {
			httpx.Forbidden(w, r)
			return
		}
		list, err := auth.ListLocalSessions(r.Context(), a.opts.Store)
		if err != nil {
			httpx.ServiceUnavailable(w, r, "Local sessions")
			return
		}
		httpx.JSON(w, 200, map[string]any{"sessions": list})
	case http.MethodPost:
		if !p.Can("sessions:revoke") {
			httpx.Forbidden(w, r)
			return
		}
		var in struct {
			Action string `json:"action"`
			ID     string `json:"id"`
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1025))
		if err != nil || len(body) > 1024 || json.Unmarshal(body, &in) != nil {
			httpx.BadRequest(w, r, "Invalid local session payload.", nil)
			return
		}
		revoked := 0
		switch {
		case in.Action == "revoke-all" && in.ID == "":
			revoked, err = auth.RevokeAllLocal(r.Context(), a.opts.Store)
		case in.Action == "revoke" && in.ID != "":
			var found bool
			found, err = auth.RevokeLocalSession(r.Context(), a.opts.Store, in.ID)
			if found {
				revoked = 1
			}
		default:
			httpx.BadRequest(w, r, "Invalid local session payload.", nil)
			return
		}
		if err != nil {
			httpx.ServiceUnavailable(w, r, "Local sessions")
			return
		}
		a.opts.Log().Info("Local sessions revoked.", "event", "audit.local-sessions-revoked", "actor_kind", p.Kind, "actor_id", p.ID,
			"session_id", in.ID, "count", revoked)
		httpx.JSON(w, 200, map[string]any{"revoked": revoked})
	default:
		// Checked here, not by the route table's gate: the caller is authenticated first, and the 405 carries Cache-Control.
		httpx.MethodNotAllowed(w, r, "GET, POST")
	}
}
