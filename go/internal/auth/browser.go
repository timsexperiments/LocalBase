package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
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
	"time"

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
	maxLoginStates    = 128
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
	states map[string]loginState
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
	b := &Browser{Store: st, Root: rootDir, Config: cfg, Log: log, Client: &http.Client{Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects are not followed") }},
		oidc: map[string]*oidcRuntime{}, states: map[string]loginState{}}
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
	if !b.Enabled() {
		return nil, ""
	}
	if b.Config.Provider.Kind == "cloudflare-access" {
		token := r.Header.Get("Cf-Access-Jwt-Assertion")
		if token == "" {
			return nil, ""
		}
		issuer := "https://" + b.Config.Provider.TeamDomain
		claims, err := b.cfJWKS.Verify(r.Context(), token, VerifyOptions{Algorithms: []string{"RS256"}, Issuer: issuer,
			Audience: b.Config.Provider.Audience, Required: []string{"exp", "sub", "email"}})
		if err != nil {
			return nil, ""
		}
		sub, _ := claims["sub"].(string)
		email, _ := claims["email"].(string)
		typ, _ := claims["type"].(string)
		_, cn := claims["common_name"]
		_, sti := claims["service_token_id"]
		if !asciiSubRe.MatchString(sub) || ValidEmail(email) == "" || strings.HasPrefix(strings.ToLower(email), "non_identity@") ||
			typ != "app" || cn || sti || claims["service_token_status"] == true {
			return nil, ""
		}
		return &Identity{Issuer: issuer, Subject: sub, VerifiedEmail: email}, ownerID("cloudflare-access", issuer, sub)
	}
	token := cookie(r, SessionCookie)
	if token == "" {
		return nil, ""
	}
	s, err := FindSession(r.Context(), b.Store, token, b.sessionIssuers())
	if err != nil || s == nil {
		return nil, ""
	}
	return &s.Identity, s.OwnerID
}

// Authorize resolves an identity's permissions: the policy when one is
// configured, else the configuration's permissions.
func (b *Browser) Authorize(ctx context.Context, id *Identity) *Decision {
	d, err := ResolvePolicy(ctx, b.Store, *id, true)
	if err != nil {
		b.Log.Error("Browser access policy lookup failed.", "event", "auth.policy-failed", "error", err.Error())
		return &Decision{MatchedRoles: []string{}, Permissions: []string{}}
	}
	if d == nil {
		return &Decision{MatchedRoles: []string{}, Permissions: b.Config.Permissions}
	}
	return d
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

const pageStyle = `body{font:16px system-ui;margin:0;background:#201e25;color:#eee9f1}main{max-width:28rem;margin:12vh auto;padding:1.5rem}h1{font-size:1.6rem}nav,form{display:grid;gap:.75rem}a{color:inherit;text-decoration:none;border:1px solid #453b4c;border-radius:.75rem;padding:1rem;background:#2b2730}a:focus,a:hover{border-color:#a4d3bc}input,button{font:inherit;border:1px solid #453b4c;border-radius:.75rem;padding:1rem;background:#2b2730;color:inherit;min-height:44px}button{background:#a4d3bc;color:#14241d;border:0;font-weight:650}button:disabled{opacity:.5}`

func page(w http.ResponseWriter, body, csp, referrer string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", referrer)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	fmt.Fprintf(w, `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Sign in to LocalBase</title><style>%s</style><main>%s</main></html>`,
		pageStyle, body)
}

const formCSP = "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

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

func (b *Browser) pruneStates() {
	t := time.Now()
	for k, s := range b.states {
		if !s.expires.After(t) {
			delete(b.states, k)
		}
	}
	for len(b.states) >= maxLoginStates {
		for k := range b.states {
			delete(b.states, k)
			break
		}
	}
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
		for _, reg := range regs {
			fmt.Fprintf(&links, `<a href="/app/login?provider=%s">%s</a>`, url.QueryEscape(reg.ID), html.EscapeString(reg.Name))
		}
		page(w, "<h1>Sign in to LocalBase</h1><nav>"+links.String()+"</nav>",
			"default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'", "no-referrer")
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
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "Identity provider not found.", 404)
		return
	}
	if reg.Kind == "magic-link" {
		page(w, fmt.Sprintf(`<h1>%s</h1><p>Enter your invited email address to receive a sign-in link.</p><form method="post" action="%s?provider=%s"><label>Email<input name="email" type="email" autocomplete="email" maxlength="320" required></label><button type="submit">Email sign-in link</button></form>`,
			html.EscapeString(reg.Name), MagicRequestPath, url.QueryEscape(reg.ID)), formCSP, "same-origin")
		return
	}
	rt := b.oidc[reg.ID]
	m, err := b.metadata(r.Context(), rt)
	if err != nil {
		Deny(w, 503)
		return
	}
	state, verifier, nonce := RandomToken(32), RandomToken(64), RandomToken(32)
	b.mu.Lock()
	b.pruneStates()
	b.states[state] = loginState{registration: reg.ID, verifier: verifier, nonce: nonce, expires: time.Now().Add(loginStateTTL)}
	b.mu.Unlock()
	authz, _ := url.Parse(m.AuthorizationEndpoint)
	q := authz.Query()
	for k, v := range map[string]string{"response_type": "code", "client_id": reg.ClientID,
		"redirect_uri": b.Config.Origin + OIDCCallbackPath, "scope": "openid profile email", "state": state, "nonce": nonce,
		"code_challenge": s256(verifier), "code_challenge_method": "S256"} {
		q.Set(k, v)
	}
	authz.RawQuery = q.Encode()
	redirect(w, authz.String(), secureCookie(loginStateCookie, state, int(loginStateTTL/time.Second)))
}

// CompleteLogin serves the OIDC callback.
func (b *Browser) CompleteLogin(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	stateValue := q.Get("state")
	b.mu.Lock()
	state, ok := b.states[stateValue]
	delete(b.states, stateValue)
	b.mu.Unlock()
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
	id, owner, err := b.exchange(r.Context(), rt, code, state)
	if err != nil {
		b.Log.Warn("OpenID Connect sign-in failed.", "event", "auth.signin-failed", "error", err.Error())
		failed()
		return
	}
	b.establish(w, r, *id, owner)
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

func (b *Browser) establish(w http.ResponseWriter, r *http.Request, id Identity, owner string) {
	token, err := CreateSession(r.Context(), b.Store, owner, id)
	if err != nil {
		redirect(w, b.Config.Origin+"/app?signin=failed", clearCookie(loginStateCookie))
		return
	}
	redirect(w, b.Config.Origin+"/app", clearCookie(loginStateCookie), secureCookie(SessionCookie, token, sessionCookieMaxAge))
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
		page(w, "<h1>Check your email</h1><p>If that address can sign in, a link is on its way. It expires in 15 minutes.</p>", formCSP, "same-origin")
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
	go func() {
		ctx := context.Background()
		token, to, err := IssueMagicLink(ctx, b.Store, email)
		if err != nil || token == "" {
			return
		}
		if err := deliver(to, b.Config.Origin+MagicCallbackPath+"#"+token); err != nil {
			RevokeMagicLink(ctx, b.Store, token)
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
	csp := fmt.Sprintf("default-src 'none'; style-src 'unsafe-inline'; script-src 'sha256-%s'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'",
		base64.StdEncoding.EncodeToString(sum[:]))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	fmt.Fprintf(w, `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Sign in to LocalBase</title><style>%s</style><main><h1>Sign in to LocalBase</h1><p>Continue to use this one-time sign-in link.</p><form method="post" action="%s"><input name="token" type="hidden"><button type="submit">Sign in</button></form></main><script>%s</script></html>`,
		pageStyle, MagicCallbackPath, redeemScript)
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
			DeleteSession(r.Context(), b.Store, token)
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
