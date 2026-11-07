package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Browser sign-in (ui/access.ts, ui/oidc-session.ts): Cloudflare Access JWT
// assertions, or direct OIDC (authorization code + PKCE + nonce) and magic
// links with database sessions. Cookie names, session rows and owner ids
// match TS, so sessions survive a switch between the implementations.

const (
	loginStateCookie  = "__Host-localbase-login-state"
	SessionCookie     = "__Host-localbase-session"
	loginStateTTL     = 10 * time.Minute
	loginStateRandom  = 24
	maxSpentStates    = 4096
	magicLinkWorkers  = 4
	maxIdPResponse    = 64 * 1024
	OIDCCallbackPath  = "/oidc/callback"
	MagicCallbackPath = "/magic-link/callback"
	MagicRequestPath  = "/app/login/magic"
)

var (
	allowedAlgs   = []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA"}
	cookieValueRe = regexp.MustCompile(`^[A-Za-z0-9_-]{32,128}$`)
	asciiSubRe    = regexp.MustCompile(`^[\x00-\x7F]{1,255}$`)
)

// Browser is the browser-identity layer of one gateway process. The access
// configuration is read at startup; changes need a restart, as in TS.
type Browser struct {
	Store  *store.Store
	Root   string
	Config *BrowserAccess // nil: browser access off
	Log    *slog.Logger
	Client *http.Client

	cfJWKS *JWKS
	oidc   map[string]*oidcRuntime
	mu     sync.Mutex
	// stateKey signs login states. It is made at start and held only in memory, so it is
	// in no file and no backup, and a restart ends a sign-in in progress, as it always did.
	stateKey []byte
	// spent holds the states of callbacks being worked on or finished, until they would
	// have expired anyway.
	spent map[string]time.Time
	// magicWork bounds the magic-link requests being worked on, since anyone can post one.
	magicWork chan struct{}
	// magicDropped is when a dropped request was last logged, as Unix seconds.
	magicDropped atomic.Int64
	// magicFailed is when a link that could not be issued was last logged, as Unix seconds.
	magicFailed atomic.Int64
	// failedLog keeps what failed sign-ins write to the log small.
	failedLog *obs.Sampler
}

type oidcRuntime struct {
	reg  Registration
	mu   sync.Mutex
	meta *oidcMetadata
	keys *JWKS
}

type oidcMetadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	ResponseTypes         []string `json:"response_types_supported"`
	CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
	SigningAlgs           []string `json:"id_token_signing_alg_values_supported"`
	TokenAuthMethods      []string `json:"token_endpoint_auth_methods_supported"`
}

type loginState struct {
	registration, verifier, nonce string
	expires                       time.Time
}

// NewBrowser prepares sign-in for a loaded configuration.
func NewBrowser(st *store.Store, rootDir string, cfg *BrowserAccess, log *slog.Logger) *Browser {
	b := &Browser{magicWork: make(chan struct{}, magicLinkWorkers), failedLog: &obs.Sampler{Every: time.Minute, Burst: 20}, Store: st, Root: rootDir, Config: cfg, Log: log, Client: &http.Client{Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects are not followed") }},
		oidc: map[string]*oidcRuntime{}, spent: map[string]time.Time{}, stateKey: make([]byte, 32)}
	if _, err := rand.Read(b.stateKey); err != nil {
		panic(err) // never, since Go 1.24
	}
	if cfg == nil {
		return b
	}
	if cfg.Provider.Kind == "cloudflare-access" {
		b.cfJWKS = &JWKS{URL: "https://" + cfg.Provider.TeamDomain + "/cdn-cgi/access/certs", Client: b.Client}
	}
	for _, r := range cfg.Provider.Registrations {
		if r.Kind == "oidc" {
			b.oidc[r.ID] = &oidcRuntime{reg: r}
		}
	}
	return b
}

// Enabled reports whether browser sign-in is configured.
func (b *Browser) Enabled() bool { return b != nil && b.Config != nil }

// Direct reports whether sign-in uses LocalBase sessions (OIDC, magic link).
func (b *Browser) Direct() bool { return b.Enabled() && b.Config.Provider.Kind == "direct" }

// HasMagicLink reports a magic-link registration.
func (b *Browser) HasMagicLink() *Registration {
	if !b.Direct() {
		return nil
	}
	for i, r := range b.Config.Provider.Registrations {
		if r.Kind == "magic-link" {
			return &b.Config.Provider.Registrations[i]
		}
	}
	return nil
}

func ownerID(parts ...string) string {
	j, _ := json.Marshal(parts)
	sum := sha256.Sum256(j)
	return "browser:" + hex.EncodeToString(sum[:])
}

func (b *Browser) sessionIssuers() map[string]bool {
	out := map[string]bool{}
	for _, r := range b.Config.Provider.Registrations {
		switch r.Kind {
		case "oidc":
			out[r.Issuer] = true
		case "magic-link":
			out[MagicIssuer(b.Config.Origin)] = true
		}
	}
	return out
}

func cookie(r *http.Request, name string) string {
	var found []string
	for _, c := range r.Cookies() {
		if c.Name == name {
			found = append(found, c.Value)
		}
	}
	if len(found) != 1 || !cookieValueRe.MatchString(found[0]) {
		return ""
	}
	return found[0]
}

// Authenticate resolves the request's browser identity, if any.
func (b *Browser) Authenticate(r *http.Request) (*Identity, string) {
	id, owner, _ := b.AuthenticateChecked(r)
	return id, owner
}

// AuthenticateChecked is Authenticate, and says when a session couldn't be read: that isn't the same
// as having no session, and shouldn't send a signed-in person to sign in again.
func (b *Browser) AuthenticateChecked(r *http.Request) (*Identity, string, error) {
	if !b.Enabled() {
		return nil, "", nil
	}
	if b.Config.Provider.Kind == "cloudflare-access" {
		token := r.Header.Get("Cf-Access-Jwt-Assertion")
		if token == "" {
			return nil, "", nil
		}
		issuer := "https://" + b.Config.Provider.TeamDomain
		claims, err := b.cfJWKS.Verify(r.Context(), token, VerifyOptions{Algorithms: []string{"RS256"}, Issuer: issuer,
			Audience: b.Config.Provider.Audience, Required: []string{"exp", "sub", "email"}})
		if err != nil {
			return nil, "", nil
		}
		sub, _ := claims["sub"].(string)
		email, _ := claims["email"].(string)
		typ, _ := claims["type"].(string)
		_, cn := claims["common_name"]
		_, sti := claims["service_token_id"]
		if !asciiSubRe.MatchString(sub) || ValidEmail(email) == "" || strings.HasPrefix(strings.ToLower(email), "non_identity@") ||
			typ != "app" || cn || sti || claims["service_token_status"] == true {
			return nil, "", nil
		}
		return &Identity{Issuer: issuer, Subject: sub, VerifiedEmail: email}, ownerID("cloudflare-access", issuer, sub), nil
	}
	token := cookie(r, SessionCookie)
	if token == "" {
		return nil, "", nil
	}
	s, err := FindSession(r.Context(), b.Store, token, b.sessionIssuers())
	if err != nil {
		return nil, "", err
	}
	if s == nil {
		return nil, "", nil
	}
	return &s.Identity, s.OwnerID, nil
}

// Authorize resolves an identity's permissions: the policy when one is
// configured, else the configuration's permissions.
func (b *Browser) Authorize(ctx context.Context, id *Identity) *Decision {
	d, err := b.AuthorizeChecked(ctx, id)
	if err != nil {
		return &Decision{MatchedRoles: []string{}, Permissions: []string{}}
	}
	return d
}

// AuthorizeChecked is Authorize, and says when the policy couldn't be read: that isn't the same as the
// policy granting nothing.
func (b *Browser) AuthorizeChecked(ctx context.Context, id *Identity) (*Decision, error) {
	d, err := ResolvePolicy(ctx, b.Store, *id, true)
	if err != nil {
		b.Log.Error("Browser access policy lookup failed.", "event", "auth.policy-failed", "error", err.Error())
		return nil, err
	}
	if d == nil {
		return &Decision{MatchedRoles: []string{}, Permissions: b.Config.Permissions}, nil
	}
	return d, nil
}

// ExactHost: the request is for the configured origin's host.
func (b *Browser) ExactHost(r *http.Request) bool {
	u, _ := url.Parse(b.Config.Origin)
	return r.Host == u.Host
}

// SameOrigin is the common CSRF rule for state-changing and UI API calls:
// the UI marker header, the exact host, a same-origin fetch, and the exact
// Origin on anything but GET.
func (b *Browser) SameOrigin(r *http.Request) bool {
	if !b.Enabled() || r.Header.Get("X-Localbase-Ui") != "1" || !b.ExactHost(r) {
		return false
	}
	site, origin := r.Header.Get("Sec-Fetch-Site"), r.Header.Get("Origin")
	if site != "" && site != "same-origin" {
		return false
	}
	if origin != "" && origin != b.Config.Origin {
		return false
	}
	return r.Method == http.MethodGet || origin == b.Config.Origin
}

// --- responses ------------------------------------------------------------------------

func redirect(w http.ResponseWriter, location string, cookies ...*http.Cookie) {
	for _, c := range cookies {
		http.SetCookie(w, c)
	}
	w.Header().Set("Location", location)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusSeeOther)
}

func secureCookie(name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode}
}

func clearCookie(name string) *http.Cookie {
	c := secureCookie(name, "", 0)
	c.MaxAge = -1
	return c
}

// Deny is the TS UI access failure body.
func Deny(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write([]byte(`{"error":{"message":"UI access denied.","type":"authentication_error","code":"ui_access_denied"}}`))
}

// Stylesheet is the app's style sheet, with its version. The web UI sets it, so these pages
// look like the rest of the app.
var Stylesheet = "/app/assets/app.css"

// pageOpen is the head of a sign-in page: the same page as the app's other sign-in pages.
func pageOpen() string {
	return `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Sign in to LocalBase</title><link rel="stylesheet" href="` + html.EscapeString(Stylesheet) + `"><main class="welcome signin"><span aria-hidden="true" class="welcome-mark"><svg viewBox="0 0 24 24" class="mark"><path fill="currentColor" d="M3 22V11a9 9 0 0 1 18 0v11Z"/><path class="mark-light" d="M6.6 22V11a5.4 5.4 0 0 1 10.8 0v11Z"/></svg></span>`
}

func page(w http.ResponseWriter, body, csp, referrer string) {
	pageAt(w, http.StatusOK, body, csp, referrer)
}

func pageAt(w http.ResponseWriter, status int, body, csp, referrer string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", referrer)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	fmt.Fprint(w, pageOpen()+body+"</main></html>")
}

const formCSP = "default-src 'none'; style-src 'self'; font-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// --- login ------------------------------------------------------------------------------

func s256(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func httpsURL(v string) bool {
	u, err := url.Parse(v)
	return err == nil && u.Scheme == "https" && u.User == nil && u.Fragment == "" && u.Host != ""
}

func (b *Browser) boundedJSON(resp *http.Response, v any) error {
	if resp.ContentLength > maxIdPResponse {
		return errors.New("Identity provider response exceeded its size limit.")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIdPResponse+1))
	if err != nil || len(body) > maxIdPResponse {
		return errors.New("Identity provider response exceeded its size limit.")
	}
	return json.Unmarshal(body, v)
}

func (b *Browser) metadata(ctx context.Context, rt *oidcRuntime) (*oidcMetadata, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.meta != nil {
		return rt.meta, nil
	}
	u, _ := url.Parse(rt.reg.Issuer)
	u.Path = strings.TrimSuffix(u.Path, "/") + "/.well-known/openid-configuration"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	req.Header.Set("Accept", "application/json")
	resp, err := b.Client.Do(req)
	if err != nil {
		return nil, errors.New("OpenID Connect discovery failed.")
	}
	defer resp.Body.Close()
	var m oidcMetadata
	if resp.StatusCode != 200 || b.boundedJSON(resp, &m) != nil {
		return nil, errors.New("OpenID Connect discovery failed.")
	}
	okAlg := false
	for _, a := range m.SigningAlgs {
		okAlg = okAlg || contains(allowedAlgs, a)
	}
	if m.Issuer != rt.reg.Issuer || !contains(m.ResponseTypes, "code") || !contains(m.CodeChallengeMethods, "S256") ||
		!httpsURL(m.AuthorizationEndpoint) || !httpsURL(m.TokenEndpoint) || !httpsURL(m.JWKSURI) || !okAlg {
		return nil, errors.New("OpenID Connect discovery is incompatible.")
	}
	auth := "none"
	if rt.reg.ClientAuthentication.Kind == "client-secret-basic" {
		auth = "client_secret_basic"
	}
	supported := m.TokenAuthMethods
	if supported == nil {
		supported = []string{"client_secret_basic"}
	}
	if !contains(supported, auth) {
		return nil, errors.New("OpenID Connect token endpoint authentication is incompatible.")
	}
	rt.meta = &m
	rt.keys = &JWKS{URL: m.JWKSURI, Client: b.Client}
	return rt.meta, nil
}

// A sign-in in progress keeps nothing on the server, so requests from anyone can't push
// out someone else's. The state carries its own expiry and a MAC, the PKCE verifier and the
// nonce are derived from it under a key that stays here, and the browser holds it in a
// cookie. A state that has signed someone in is remembered, so it works once. A callback
// that fails remembers nothing, so failing them can't push that record out either.

func (b *Browser) loginStateMAC(purpose, registration string, data []byte) []byte {
	m := hmac.New(sha256.New, b.stateKey)
	fmt.Fprintf(m, "%s\x00%s\x00", purpose, registration)
	m.Write(data)
	return m.Sum(nil)
}

// newLoginState makes the state value for a registration.
func (b *Browser) newLoginState(registration string, expires time.Time) string {
	body := make([]byte, loginStateRandom+8)
	if _, err := rand.Read(body[:loginStateRandom]); err != nil {
		panic(err) // never, since Go 1.24
	}
	binary.BigEndian.PutUint64(body[loginStateRandom:], uint64(expires.Unix()))
	return base64.RawURLEncoding.EncodeToString(append(body, b.loginStateMAC("state", registration, body)...))
}

// loginStateOf gives what a state value stands for, if this process made it for a
// registration that is still configured.
func (b *Browser) loginStateOf(value string) (loginState, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) != loginStateRandom+8+sha256.Size {
		return loginState{}, false
	}
	body, mac := raw[:loginStateRandom+8], raw[loginStateRandom+8:]
	for id := range b.oidc {
		if hmac.Equal(mac, b.loginStateMAC("state", id, body)) {
			derive := func(purpose string) string {
				return base64.RawURLEncoding.EncodeToString(b.loginStateMAC(purpose, id, body))
			}
			return loginState{registration: id, verifier: derive("verifier"), nonce: derive("nonce"),
				expires: time.Unix(int64(binary.BigEndian.Uint64(body[loginStateRandom:])), 0)}, true
		}
	}
	return loginState{}, false
}

// spendLoginState reports whether this callback may use a state: nobody has finished with
// it and nobody else is using it now. Only completed sign-ins stay here, so forgetting one
// takes thousands of real sign-ins within ten minutes, and even then the provider's
// single-use code holds.
func (b *Browser) spendLoginState(value string, expires time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, used := b.spent[value]; used {
		return false
	}
	t := time.Now()
	for k, until := range b.spent {
		if !until.After(t) {
			delete(b.spent, k)
		}
	}
	for len(b.spent) >= maxSpentStates {
		for k := range b.spent {
			delete(b.spent, k)
			break
		}
	}
	b.spent[value] = expires
	return true
}

// unspendLoginState gives a state back after a callback that signed nobody in.
func (b *Browser) unspendLoginState(value string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.spent, value)
}

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

func (b *Browser) exchange(ctx context.Context, rt *oidcRuntime, code string, state loginState) (*Identity, string, error) {
	m, err := b.metadata(ctx, rt)
	if err != nil {
		return nil, "", err
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {b.Config.Origin + OIDCCallbackPath}, "code_verifier": {state.verifier}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, m.TokenEndpoint, nil)
	if rt.reg.ClientAuthentication.Kind == "none" {
		form.Set("client_id", rt.reg.ClientID)
	} else {
		esc := func(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "%20", "+") }
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(esc(rt.reg.ClientID)+":"+esc(rt.reg.ClientAuthentication.ClientSecret))))
	}
	req.Body = io.NopCloser(strings.NewReader(form.Encode()))
	req.ContentLength = int64(len(form.Encode()))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := b.Client.Do(req)
	if err != nil {
		return nil, "", errors.New("OpenID Connect token exchange failed.")
	}
	defer resp.Body.Close()
	var tok struct {
		IDToken string `json:"id_token"`
	}
	if resp.StatusCode != 200 || b.boundedJSON(resp, &tok) != nil || tok.IDToken == "" || len(tok.IDToken) > 48*1024 {
		return nil, "", errors.New("OpenID Connect token exchange failed.")
	}
	var algs []string
	for _, a := range m.SigningAlgs {
		if contains(allowedAlgs, a) {
			algs = append(algs, a)
		}
	}
	claims, err := rt.keys.Verify(ctx, tok.IDToken, VerifyOptions{Algorithms: algs, Issuer: rt.reg.Issuer,
		Audience: rt.reg.ClientID, Required: []string{"exp", "iat", "sub", "nonce"}})
	if err != nil {
		return nil, "", err
	}
	sub, _ := claims["sub"].(string)
	nonce, _ := claims["nonce"].(string)
	if !asciiSubRe.MatchString(sub) || subtle.ConstantTimeCompare([]byte(nonce), []byte(state.nonce)) != 1 {
		return nil, "", errors.New("OpenID Connect nonce did not match.")
	}
	azp, hasAzp := claims["azp"].(string)
	multiAud := false
	if list, ok := claims["aud"].([]any); ok && len(list) > 1 {
		multiAud = true
	}
	if (multiAud && azp != rt.reg.ClientID) || (!multiAud && hasAzp && azp != rt.reg.ClientID) {
		return nil, "", errors.New("OpenID Connect authorized party did not match.")
	}
	id := &Identity{Issuer: rt.reg.Issuer, Subject: sub}
	if claims["email_verified"] == true {
		if email, _ := claims["email"].(string); ValidEmail(email) != "" {
			id.VerifiedEmail = email
		}
	}
	return id, ownerID("oidc", rt.reg.Issuer, sub), nil
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

// noAccessPage tells someone with no permissions so, and whoever runs this LocalBase the
// one thing to do about it, with the command for how far it has been set up.
func noAccessPage(email, issuer string, hasPolicy bool) string {
	account, address := "that account", "&lt;your address&gt;"
	if email != "" {
		account = html.EscapeString(email)
		address = account
	}
	command := "local-base access policy starter --email " + address
	then := "If this LocalBase is yours, run this on its computer, then sign in again:"
	if hasPolicy {
		command = "local-base access users invite --email " + address + " --roles &lt;role&gt;"
		then = "Whoever runs this LocalBase can give it access, by running this on its computer:"
	}
	if issuer != "" {
		command += " --issuer " + html.EscapeString(issuer)
	}
	return "<h1>This account has no access here</h1><p>You signed in as " + account + ", but this LocalBase gives it nothing, so you were not signed in.</p><p>" + then + "</p><pre>" + command + "</pre>"
}

func formOK(r *http.Request, origin string) bool {
	site := r.Header.Get("Sec-Fetch-Site")
	return r.Header.Get("Origin") == origin && (site == "" || site == "same-origin") &&
		strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/x-www-form-urlencoded")
}

func readForm(r *http.Request, max int64, field string) (string, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil || int64(len(body)) > max {
		return "", false
	}
	v, err := url.ParseQuery(string(body))
	if err != nil || len(v) != 1 || len(v[field]) != 1 {
		return "", false
	}
	return v[field][0], true
}

// RequestMagicLink serves POST /app/login/magic. The response is the same
// whether or not the address can sign in.
func (b *Browser) RequestMagicLink(w http.ResponseWriter, r *http.Request, deliver func(to, link string) error) {
	done := func() {
		page(w, `<h1 class="welcome-title">Check your email</h1><p class="mt-3 text-muted">If that address can sign in, a link is on its way. It works once, for 15 minutes, and asking again before then doesn’t send another.</p>`, formCSP, "same-origin")
	}
	reg := b.HasMagicLink()
	if reg == nil || r.URL.Query().Get("provider") != reg.ID || !formOK(r, b.Config.Origin) {
		done()
		return
	}
	email, ok := readForm(r, 1024, "email")
	if !ok {
		done()
		return
	}
	if ValidEmail(email) == "" {
		done()
		return
	}
	select {
	case b.magicWork <- struct{}{}:
	default:
		// The page reads the same either way. Whoever asked can ask again.
		now := time.Now().Unix()
		if last := b.magicDropped.Load(); now-last >= 60 && b.magicDropped.CompareAndSwap(last, now) {
			b.Log.Warn("Magic-link requests were dropped because too many are being worked on.", "event", "auth.magic-link-dropped")
		}
		done()
		return
	}
	go func() {
		defer func() { <-b.magicWork }()
		ctx := context.Background()
		token, to, err := IssueMagicLink(ctx, b.Store, email, MagicIssuer(b.Config.Origin))
		if err != nil {
			// Sparingly, since anyone can ask as often as the database keeps failing.
			now := time.Now().Unix()
			if last := b.magicFailed.Load(); now-last >= 60 && b.magicFailed.CompareAndSwap(last, now) {
				b.Log.Error("A magic link could not be issued.", "event", "auth.magic-link-issue-failed", "error", err.Error())
			}
			return
		}
		if token == "" {
			return
		}
		if err := deliver(to, b.Config.Origin+MagicCallbackPath+"#"+token); err != nil {
			if rerr := RevokeMagicLink(ctx, b.Store, token); rerr != nil {
				b.Log.Error("Could not revoke a magic link that was not delivered.", "event", "auth.magic-link-revoke-failed", "error", rerr.Error())
			}
			b.Log.Warn("Magic-link email delivery failed.", "event", "auth.magic-link-delivery-failed", "error", err.Error())
		}
	}()
	done()
}

const redeemScript = `const token=location.hash.slice(1);history.replaceState(null,"",location.pathname);const input=document.querySelector('input[name="token"]');const button=document.querySelector("button");if(/^[A-Za-z0-9_-]{43}$/.test(token)){input.value=token}else{button.disabled=true}`

// MagicLinkPage serves GET /magic-link/callback: the token stays in the URL
// fragment and is posted by a hash-pinned script.
func (b *Browser) MagicLinkPage(w http.ResponseWriter) {
	sum := sha256.Sum256([]byte(redeemScript))
	csp := fmt.Sprintf("default-src 'none'; style-src 'self'; font-src 'self'; script-src 'sha256-%s'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'",
		base64.StdEncoding.EncodeToString(sum[:]))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	fmt.Fprintf(w, pageOpen()+`<h1 class="welcome-title">Sign in to LocalBase</h1><p class="mt-3 text-muted">Continue to use this one-time sign-in link.</p><form method="post" action="%s" class="mt-5"><input name="token" type="hidden"><button class="btn btn-primary w-full" type="submit">Sign in</button></form></main><script>%s</script></html>`,
		MagicCallbackPath, redeemScript)
}

// CompleteMagicLink serves POST /magic-link/callback.
func (b *Browser) CompleteMagicLink(w http.ResponseWriter, r *http.Request) {
	failed := func() { redirect(w, b.Config.Origin+"/app?signin=failed") }
	if b.HasMagicLink() == nil || !formOK(r, b.Config.Origin) {
		failed()
		return
	}
	token, ok := readForm(r, 256, "token")
	if !ok || len(token) > 128 {
		failed()
		return
	}
	id, err := ConsumeMagicLink(r.Context(), b.Store, token, MagicIssuer(b.Config.Origin))
	if err != nil || id == nil {
		failed()
		return
	}
	b.establish(w, r, *id, ownerID("magic-link", id.Issuer, id.Subject))
}

// Logout serves POST /app/logout.
func (b *Browser) Logout(w http.ResponseWriter, r *http.Request) {
	if b.Direct() {
		if token := cookie(r, SessionCookie); token != "" {
			if err := DeleteSession(r.Context(), b.Store, token); err != nil {
				// The session is still good, so the person isn't told otherwise and keeps the cookie.
				b.Log.Error("Could not end a browser session; it stays valid until it expires.", "event", "auth.session-delete-failed", "error", err.Error())
				w.Header().Set("Retry-After", "5")
				Deny(w, http.StatusServiceUnavailable)
				return
			}
		}
		http.SetCookie(w, clearCookie(SessionCookie))
	}
	w.WriteHeader(http.StatusNoContent)
}

// CloudflareLogout is where a Cloudflare Access session signs out.
func (b *Browser) CloudflareLogout() string {
	if b.Enabled() && b.Config.Provider.Kind == "cloudflare-access" {
		return "/cdn-cgi/access/logout"
	}
	return ""
}

// SameOriginStream is SameOrigin for a GET event stream (DESIGN §21.3). An
// EventSource cannot add X-Localbase-Ui, so it rests on the session cookie,
// the exact host, and the browser's same-origin markers. It is read-only:
// without CORS headers no other site can read the stream.
func (b *Browser) SameOriginStream(r *http.Request) bool {
	if !b.Enabled() || r.Method != http.MethodGet || !b.ExactHost(r) {
		return false
	}
	site, origin := r.Header.Get("Sec-Fetch-Site"), r.Header.Get("Origin")
	if site != "" && site != "same-origin" {
		return false
	}
	return origin == "" || origin == b.Config.Origin
}
