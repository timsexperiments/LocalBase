package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

const testOrigin = "https://lb.example.com"

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "localbase.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// fakeIdP is an OpenID provider that signs ID tokens with an RSA key and
// checks PKCE.
type fakeIdP struct {
	srv       *httptest.Server
	key       *rsa.PrivateKey
	challenge string // from the authorization request
	nonce     string
	claims    map[string]any // overrides
}

func newFakeIdP(t *testing.T) *fakeIdP {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	f := &fakeIdP{key: key}
	mux := http.NewServeMux()
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/authorize",
			"token_endpoint": f.srv.URL + "/token", "jwks_uri": f.srv.URL + "/jwks", "response_types_supported": []string{"code"},
			"code_challenge_methods_supported": []string{"S256"}, "id_token_signing_alg_values_supported": []string{"RS256"},
			"token_endpoint_auth_methods_supported": []string{"client_secret_basic"}})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		enc := base64.RawURLEncoding
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
			"n": enc.EncodeToString(key.N.Bytes()), "e": enc.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		user, pass, _ := r.BasicAuth()
		if user != "cid" || pass != "shh" || r.Form.Get("code") != "good-code" || s256(r.Form.Get("code_verifier")) != f.challenge ||
			r.Form.Get("redirect_uri") != testOrigin+OIDCCallbackPath {
			w.WriteHeader(400)
			return
		}
		claims := map[string]any{"iss": f.srv.URL, "aud": "cid", "sub": "user-1", "nonce": f.nonce, "email": "Owner@Example.com",
			"email_verified": true, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
		for k, v := range f.claims {
			if v == nil {
				delete(claims, k)
			} else {
				claims[k] = v
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"id_token": f.sign(claims)})
	})
	return f
}

func (f *fakeIdP) sign(claims map[string]any) string {
	enc := base64.RawURLEncoding
	h, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	p, _ := json.Marshal(claims)
	signed := enc.EncodeToString(h) + "." + enc.EncodeToString(p)
	sum := sha256.Sum256([]byte(signed))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	return signed + "." + enc.EncodeToString(sig)
}

func browserFor(t *testing.T, st *store.Store, regs ...Registration) *Browser {
	cfg := &BrowserAccess{Provider: Provider{Kind: "direct", Registrations: regs}, Origin: testOrigin,
		Permissions: []string{"inference:chat", "models:read"}}
	return NewBrowser(st, t.TempDir(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func get(target string, cookies ...*http.Cookie) *http.Request {
	r := httptest.NewRequest("GET", target, nil)
	r.Host = "lb.example.com"
	for _, c := range cookies {
		r.AddCookie(c)
	}
	return r
}

func cookieNamed(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// login runs StartLogin and the callback; mutate may tamper with the
// authorization response.
func login(t *testing.T, b *Browser, f *fakeIdP, mutate func(q url.Values, state *http.Cookie) *http.Cookie) *http.Response {
	w := httptest.NewRecorder()
	b.StartLogin(w, get("/app/login"))
	resp := w.Result()
	if resp.StatusCode != 303 {
		t.Fatalf("login: %d %s", resp.StatusCode, w.Body)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	q := loc.Query()
	if !strings.HasPrefix(loc.String(), f.srv.URL+"/authorize?") || q.Get("code_challenge_method") != "S256" ||
		q.Get("redirect_uri") != testOrigin+OIDCCallbackPath || q.Get("client_id") != "cid" {
		t.Fatalf("authorization redirect %s", loc)
	}
	f.challenge, f.nonce = q.Get("code_challenge"), q.Get("nonce")
	state := cookieNamed(resp, loginStateCookie)
	if state == nil || !state.Secure || !state.HttpOnly || state.Value != q.Get("state") {
		t.Fatalf("state cookie %+v", state)
	}
	back := url.Values{"state": {q.Get("state")}, "code": {"good-code"}}
	if mutate != nil {
		state = mutate(back, state)
	}
	w = httptest.NewRecorder()
	b.CompleteLogin(w, get(OIDCCallbackPath+"?"+back.Encode(), state))
	return w.Result()
}

func TestOIDCSignIn(t *testing.T) {
	st := testStore(t)
	f := newFakeIdP(t)
	reg := Registration{Kind: "oidc", ID: "idp", Name: "IdP", Issuer: f.srv.URL, ClientID: "cid",
		ClientAuthentication: &ClientAuth{Kind: "client-secret-basic", ClientSecret: "shh"}}
	b := browserFor(t, st, reg)
	b.Client = f.srv.Client()

	resp := login(t, b, f, nil)
	session := cookieNamed(resp, SessionCookie)
	if resp.StatusCode != 303 || resp.Header.Get("Location") != testOrigin+"/app" || session == nil || session.Value == "" {
		t.Fatalf("callback: %d %s %+v", resp.StatusCode, resp.Header.Get("Location"), session)
	}
	id, owner := b.Authenticate(get("/app/session", session))
	if id == nil || id.Subject != "user-1" || id.Issuer != f.srv.URL || id.VerifiedEmail != "Owner@Example.com" ||
		owner != ownerID("oidc", f.srv.URL, "user-1") {
		t.Fatalf("identity %+v %s", id, owner)
	}
	if d := b.Authorize(context.Background(), id); strings.Join(d.Permissions, ",") != "inference:chat,models:read" {
		t.Errorf("permissions %v", d.Permissions)
	}

	// Logout ends the session.
	w := httptest.NewRecorder()
	b.Logout(w, get("/app/logout", session))
	if w.Code != 204 {
		t.Errorf("logout %d", w.Code)
	}
	if id, _ := b.Authenticate(get("/app/session", session)); id != nil {
		t.Error("session survived logout")
	}

	failures := map[string]func(q url.Values, state *http.Cookie) *http.Cookie{
		"state cookie mismatch": func(q url.Values, s *http.Cookie) *http.Cookie { return &http.Cookie{Name: s.Name, Value: "other"} },
		"no state cookie":       func(q url.Values, s *http.Cookie) *http.Cookie { return &http.Cookie{Name: "x", Value: "y"} },
		"provider error":        func(q url.Values, s *http.Cookie) *http.Cookie { q.Set("error", "access_denied"); return s },
		"bad code":              func(q url.Values, s *http.Cookie) *http.Cookie { q.Set("code", "bad"); return s },
	}
	for name, mutate := range failures {
		resp := login(t, b, f, mutate)
		if resp.Header.Get("Location") != testOrigin+"/app?signin=failed" || cookieNamed(resp, SessionCookie) != nil {
			t.Errorf("%s: %d %s", name, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	claimFailures := map[string]map[string]any{
		"wrong nonce":    {"nonce": "x"},
		"wrong audience": {"aud": "other"},
		"expired":        {"exp": time.Now().Add(-time.Minute).Unix()},
		"wrong issuer":   {"iss": "https://evil.example.com"},
		"foreign azp":    {"aud": []any{"cid", "other"}, "azp": "other"},
		"no sub":         {"sub": nil},
	}
	for name, claims := range claimFailures {
		f.claims = claims
		resp := login(t, b, f, nil)
		if resp.Header.Get("Location") != testOrigin+"/app?signin=failed" || cookieNamed(resp, SessionCookie) != nil {
			t.Errorf("%s: %d %s", name, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	// An unverified email is not carried into the identity.
	f.claims = map[string]any{"email_verified": false}
	session = cookieNamed(login(t, b, f, nil), SessionCookie)
	if id, _ := b.Authenticate(get("/app/session", session)); id == nil || id.VerifiedEmail != "" {
		t.Errorf("unverified email identity %+v", id)
	}
}

func TestMagicLinkSignIn(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	b := browserFor(t, st, Registration{Kind: "magic-link", ID: "email", Name: "Email"})
	if _, err := InviteUser(ctx, st, "friend@example.com", []string{}); err == nil {
		t.Fatal("invite without a policy role should fail")
	}
	none := ""
	if err := ApplyPolicy(ctx, st, &Policy{Roles: []Role{{Name: "chat", Description: "Chat", Permissions: []string{"inference:chat"}}},
		Bindings: []Binding{}, DefaultRole: &none}); err == nil {
		t.Fatal("unknown default role should fail")
	}
	admin := Role{Name: "admin", Description: "Admin", Permissions: []string{"access:manage"}}
	owner := Binding{Kind: "subject", Role: "admin", Issuer: "https://id.example.com", Subject: "owner"}
	if err := ApplyPolicy(ctx, st, &Policy{Roles: []Role{{Name: "chat", Description: "Chat", Permissions: []string{"inference:chat"}}, admin},
		Bindings: []Binding{owner}}); err != nil {
		t.Fatal(err)
	}
	if _, err := InviteUser(ctx, st, "Friend@Example.com", []string{"chat"}); err != nil {
		t.Fatal(err)
	}

	request := func(email string, headers map[string]string) (string, bool) {
		links := make(chan string, 1)
		body := url.Values{"email": {email}}.Encode()
		r := httptest.NewRequest("POST", MagicRequestPath+"?provider=email", strings.NewReader(body))
		r.Host = "lb.example.com"
		r.Header.Set("Origin", testOrigin)
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		b.RequestMagicLink(w, r, func(to, link string) error { links <- link; return nil })
		if w.Code != 200 || !strings.Contains(w.Body.String(), "Check your email") {
			t.Fatalf("request: %d %s", w.Code, w.Body)
		}
		select {
		case link := <-links:
			return link, true
		case <-time.After(300 * time.Millisecond):
			return "", false
		}
	}
	if _, sent := request("stranger@example.com", nil); sent {
		t.Error("link sent to an uninvited address")
	}
	if _, sent := request("friend@example.com", map[string]string{"Origin": "https://evil.example.com"}); sent {
		t.Error("link sent for a cross-origin form")
	}
	link, sent := request("FRIEND@example.com", nil)
	if !sent || !strings.HasPrefix(link, testOrigin+MagicCallbackPath+"#") {
		t.Fatalf("link %q", link)
	}
	if _, sent := request("friend@example.com", nil); sent {
		t.Error("cooldown did not hold")
	}
	token := link[strings.Index(link, "#")+1:]
	if len(token) != 43 {
		t.Errorf("token length %d", len(token))
	}
	redeem := func(token string) *http.Response {
		r := httptest.NewRequest("POST", MagicCallbackPath, strings.NewReader(url.Values{"token": {token}}.Encode()))
		r.Host = "lb.example.com"
		r.Header.Set("Origin", testOrigin)
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		b.CompleteMagicLink(w, r)
		return w.Result()
	}
	resp := redeem(token)
	session := cookieNamed(resp, SessionCookie)
	if resp.Header.Get("Location") != testOrigin+"/app" || session == nil {
		t.Fatalf("redeem: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp := redeem(token); cookieNamed(resp, SessionCookie) != nil {
		t.Error("magic link redeemed twice")
	}
	id, _ := b.Authenticate(get("/app/session", session))
	if id == nil || id.VerifiedEmail != "friend@example.com" || id.Issuer != MagicIssuer(testOrigin) {
		t.Fatalf("identity %+v", id)
	}
	// The invitation is claimed on first use and grants the invited roles.
	if d := b.Authorize(ctx, id); strings.Join(d.MatchedRoles, ",") != "chat" || strings.Join(d.Permissions, ",") != "inference:chat" {
		t.Errorf("decision %+v", d)
	}
	users, _ := ListUsers(ctx, st)
	if len(users) != 1 || users[0].Status != "active" {
		t.Errorf("users %+v", users)
	}
	// A disabled user loses access on the next request.
	if _, err := SetUserStatus(ctx, st, "friend@example.com", "disabled"); err != nil {
		t.Fatal(err)
	}
	if d := b.Authorize(ctx, id); len(d.Permissions) != 0 {
		t.Errorf("disabled user permissions %v", d.Permissions)
	}
}

func TestSameOrigin(t *testing.T) {
	b := browserFor(t, testStore(t), Registration{Kind: "magic-link", ID: "email", Name: "Email"})
	mk := func(method, host string, headers map[string]string) *http.Request {
		r := httptest.NewRequest(method, "/app/api/v1/models", nil)
		r.Host = host
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		return r
	}
	ui := map[string]string{"X-Localbase-Ui": "1"}
	cases := []struct {
		r    *http.Request
		want bool
	}{
		{mk("GET", "lb.example.com", ui), true},
		{mk("GET", "lb.example.com", nil), false},
		{mk("GET", "other.example.com", ui), false},
		{mk("POST", "lb.example.com", ui), false},
		{mk("POST", "lb.example.com", map[string]string{"X-Localbase-Ui": "1", "Origin": testOrigin}), true},
		{mk("POST", "lb.example.com", map[string]string{"X-Localbase-Ui": "1", "Origin": "https://evil.example.com"}), false},
		{mk("GET", "lb.example.com", map[string]string{"X-Localbase-Ui": "1", "Sec-Fetch-Site": "cross-site"}), false},
	}
	for i, c := range cases {
		if got := b.SameOrigin(c.r); got != c.want {
			t.Errorf("case %d: %v", i, got)
		}
	}
}
