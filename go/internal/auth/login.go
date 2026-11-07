package auth

import (
	"crypto/subtle"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// --- login ------------------------------------------------------------------------------

// StartLogin serves GET /app/login: a provider choice, a magic-link form,
// or a redirect to the identity provider.
func (b *Browser) StartLogin(w http.ResponseWriter, r *http.Request) {
	if !b.Direct() {
		redirect(w, b.Config.Origin+"/app")
		return
	}
	regs := []Registration{}
	for _, reg := range b.Config.Provider.Registrations {
		if reg.Kind != "github-oauth" {
			regs = append(regs, reg)
		}
	}
	id := r.URL.Query().Get("provider")
	_, given := r.URL.Query()["provider"]
	if !given && len(regs) > 1 {
		var links strings.Builder
		carry := ""
		if r.URL.Query().Get("switch") == "1" {
			carry = "&switch=1" // survives choosing a provider
		}
		for _, reg := range regs {
			fmt.Fprintf(&links, `<a class="btn btn-quiet w-full" href="/app/login?provider=%s%s">Continue with %s</a>`, url.QueryEscape(reg.ID), carry, html.EscapeString(reg.Name))
		}
		page(w, `<h1 class="welcome-title">Sign in to LocalBase</h1><nav class="mt-5 grid gap-2">`+links.String()+"</nav>",
			"default-src 'none'; style-src 'self'; font-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'", "no-referrer")
		return
	}
	var reg *Registration
	for i := range regs {
		if (!given && i == 0) || regs[i].ID == id {
			reg = &regs[i]
			break
		}
	}
	if reg == nil {
		if strings.Contains(r.Header.Get("Accept"), "text/html") {
			pageAt(w, http.StatusNotFound, `<h1 class="welcome-title">That way of signing in isn’t here</h1><p class="mt-3 text-muted">The link may be old.</p><a class="btn btn-primary mt-5 w-full" href="/app/login">Sign in</a>`, formCSP, "same-origin")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "Identity provider not found.", 404)
		return
	}
	if reg.Kind == "magic-link" {
		page(w, fmt.Sprintf(`<h1 class="welcome-title">Sign in to LocalBase</h1><p class="mt-3 text-muted">Enter your invited email address, and we’ll send you a sign-in link.</p><form method="post" action="%s?provider=%s" class="mt-5 grid gap-3"><label class="grid gap-1 font-semibold">Email<input class="input font-normal" name="email" type="email" autocomplete="email" maxlength="320" required></label><button class="btn btn-primary w-full" type="submit">Email me a link</button></form>`,
			MagicRequestPath, url.QueryEscape(reg.ID)), formCSP, "same-origin")
		return
	}
	rt := b.oidc[reg.ID]
	m, err := b.metadata(r.Context(), rt)
	if err != nil {
		// A person opening the page gets words; anything else gets the usual answer.
		if strings.Contains(r.Header.Get("Accept"), "text/html") {
			pageAt(w, http.StatusServiceUnavailable, `<h1 class="welcome-title">Sign-in isn’t answering</h1><p class="mt-3 text-muted">LocalBase couldn’t reach the sign-in service. Try again in a moment.</p><a class="btn btn-primary mt-5 w-full" href="/app/login">Try again</a>`, formCSP, "same-origin")
			return
		}
		Deny(w, 503)
		return
	}
	state := b.newLoginState(reg.ID, time.Now().Add(loginStateTTL))
	started, _ := b.loginStateOf(state)
	verifier, nonce := started.verifier, started.nonce
	authz, _ := url.Parse(m.AuthorizationEndpoint)
	q := authz.Query()
	for k, v := range map[string]string{"response_type": "code", "client_id": reg.ClientID,
		"redirect_uri": b.Config.Origin + OIDCCallbackPath, "scope": "openid profile email", "state": state, "nonce": nonce,
		"code_challenge": s256(verifier), "code_challenge_method": "S256"} {
		q.Set(k, v)
	}
	// "Use another account": only this request asks for the provider's chooser, so
	// normal sign-in is not one tap longer, and a provider that rejects it can only
	// affect this recovery path.
	if r.URL.Query().Get("switch") == "1" {
		q.Set("prompt", "select_account")
	}
	authz.RawQuery = q.Encode()
	redirect(w, authz.String(), secureCookie(loginStateCookie, state, int(loginStateTTL/time.Second)))
}

// CompleteLogin serves the OIDC callback.
func (b *Browser) CompleteLogin(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	stateValue := q.Get("state")
	state, ok := b.loginStateOf(stateValue)
	failed := func() { redirect(w, b.Config.Origin+"/app?signin=failed", clearCookie(loginStateCookie)) }
	cookieState := cookie(r, loginStateCookie)
	if !ok || !state.expires.After(time.Now()) || subtle.ConstantTimeCompare([]byte(stateValue), []byte(cookieState)) != 1 ||
		q.Has("error") {
		failed()
		return
	}
	code := q.Get("code")
	rt := b.oidc[state.registration]
	if code == "" || len(code) > 8192 || rt == nil {
		failed()
		return
	}
	if !b.spendLoginState(stateValue, state.expires) {
		failed()
		return
	}
	id, owner, err := b.exchange(r.Context(), rt, code, state)
	if err != nil {
		b.unspendLoginState(stateValue)
		if now := time.Now(); b.failedLog.Allow(now) { // anyone can start a sign-in and fail it
			b.Log.Warn("OpenID Connect sign-in failed.", "event", "auth.signin-failed", "error", err.Error(), "held_back", b.failedLog.Flush(now))
		}
		failed()
		return
	}
	b.establish(w, r, *id, owner)
}

// LogHeldBack says how many failed sign-ins were counted rather than logged. It's for shutdown,
// when no later failure will say so.
func (b *Browser) LogHeldBack() {
	if n := b.failedLog.Drain(time.Now()); n > 0 {
		b.Log.Warn("Failed sign-ins were counted rather than logged.", "event", "auth.signin-failed-summary", "held_back", n)
	}
}

// establish starts a session for a sign-in that is allowed something. One that isn't gets
// no session, so nothing is kept about someone this LocalBase has nothing for, and a page
// that says so. Authorizing also claims an invitation, so a first sign-in counts.
func (b *Browser) establish(w http.ResponseWriter, r *http.Request, id Identity, owner string) {
	d, err := b.AuthorizeChecked(r.Context(), &id)
	if err != nil {
		// Not knowing what they may do isn't knowing they may do nothing.
		redirect(w, b.Config.Origin+"/app?signin=failed", clearCookie(loginStateCookie))
		return
	}
	if len(d.Permissions) == 0 {
		b.Log.Info("A sign-in that has no permissions here was refused.", "event", "auth.signin-denied", "actor_id", owner)
		policy, err := LoadPolicy(r.Context(), b.Store.DB)
		http.SetCookie(w, clearCookie(loginStateCookie))
		// With several providers, the command says which one this sign-in came through.
		issuer := ""
		if len(b.Config.Issuers()) > 1 {
			issuer = id.Issuer
		}
		pageAt(w, http.StatusForbidden, noAccessPage(id.VerifiedEmail, issuer, err != nil || policy != nil), formCSP, "no-referrer")
		return
	}
	token, err := CreateSession(r.Context(), b.Store, owner, id)
	if err != nil {
		redirect(w, b.Config.Origin+"/app?signin=failed", clearCookie(loginStateCookie))
		return
	}
	b.Log.Info("Signed in.", "event", "auth.signin", "actor_id", owner, "roles", strings.Join(d.MatchedRoles, ","))
	redirect(w, b.Config.Origin+"/app", clearCookie(loginStateCookie), secureCookie(SessionCookie, token, sessionCookieMaxAge))
}
