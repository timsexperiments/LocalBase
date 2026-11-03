package auth

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Characterization of the OIDC path the live gateway signs in through, ahead of
// any cleanup in this package. It adds what TestOIDCSignIn does not cover: replayed
// and expired state, forged tokens, who is authorized, and session expiry.

func oidcFixture(t *testing.T) (*Browser, *fakeIdP) {
	t.Helper()
	f := newFakeIdP(t)
	b := browserFor(t, testStore(t), Registration{Kind: "oidc", ID: "idp", Name: "IdP", Issuer: f.srv.URL, ClientID: "cid",
		ClientAuthentication: &ClientAuth{Kind: "client-secret-basic", ClientSecret: "shh"}})
	b.Client = f.srv.Client()
	return b, f
}

func refused(t *testing.T, name string, resp *http.Response) {
	t.Helper()
	if resp.Header.Get("Location") != testOrigin+"/app?signin=failed" || cookieNamed(resp, SessionCookie) != nil {
		t.Errorf("%s: %d %s", name, resp.StatusCode, resp.Header.Get("Location"))
	}
}

// A state works once: replaying a good callback gets no second session.
func TestOIDCStateReplayed(t *testing.T) {
	b, f := oidcFixture(t)
	var back url.Values
	var state *http.Cookie
	first := login(t, b, f, func(q url.Values, s *http.Cookie) *http.Cookie {
		back, state = q, s
		return s
	})
	if cookieNamed(first, SessionCookie) == nil {
		t.Fatalf("the first callback: %d", first.StatusCode)
	}
	w := httptest.NewRecorder()
	b.CompleteLogin(w, get(OIDCCallbackPath+"?"+back.Encode(), state))
	refused(t, "replayed", w.Result())
	// A state mismatch also spends the state: the right cookie cannot follow it.
	var again url.Values
	var good *http.Cookie
	refused(t, "mismatch", login(t, b, f, func(q url.Values, s *http.Cookie) *http.Cookie {
		again, good = q, s
		return &http.Cookie{Name: s.Name, Value: strings.Repeat("o", 43)}
	}))
	w = httptest.NewRecorder()
	b.CompleteLogin(w, get(OIDCCallbackPath+"?"+again.Encode(), good))
	refused(t, "after a mismatch", w.Result())
}

func TestOIDCStateExpired(t *testing.T) {
	b, f := oidcFixture(t)
	refused(t, "expired state", login(t, b, f, func(q url.Values, s *http.Cookie) *http.Cookie {
		b.mu.Lock()
		st := b.states[q.Get("state")]
		st.expires = time.Now().Add(-time.Second)
		b.states[q.Get("state")] = st
		b.mu.Unlock()
		return s
	}))
}

func TestOIDCForgedTokens(t *testing.T) {
	b, f := oidcFixture(t)
	enc := base64.RawURLEncoding
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	segment := func(v any) string {
		raw, _ := json.Marshal(v)
		return enc.EncodeToString(raw)
	}
	for name, tamper := range map[string]func(string, map[string]any) string{
		"corrupt signature": func(token string, _ map[string]any) string {
			i := strings.LastIndex(token, ".")
			sig, _ := enc.DecodeString(token[i+1:])
			sig[0] ^= 0xff
			return token[:i+1] + enc.EncodeToString(sig)
		},
		"another key's signature": func(_ string, claims map[string]any) string {
			signed := segment(map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"}) + "." + segment(claims)
			sum := sha256.Sum256([]byte(signed))
			sig, _ := rsa.SignPKCS1v15(rand.Reader, other, crypto.SHA256, sum[:])
			return signed + "." + enc.EncodeToString(sig)
		},
		"changed claims": func(token string, claims map[string]any) string {
			parts := strings.Split(token, ".")
			claims["sub"] = "someone-else"
			return parts[0] + "." + segment(claims) + "." + parts[2]
		},
		"alg none": func(_ string, claims map[string]any) string {
			return segment(map[string]any{"alg": "none", "typ": "JWT"}) + "." + segment(claims) + "."
		},
		"HS256": func(_ string, claims map[string]any) string {
			return segment(map[string]any{"alg": "HS256", "kid": "k1", "typ": "JWT"}) + "." + segment(claims) + "." + enc.EncodeToString([]byte("sig"))
		},
		"not a token": func(string, map[string]any) string { return "garbage" },
	} {
		f.tamper = tamper
		refused(t, name, login(t, b, f, nil))
	}
}

// Signing in is not being allowed in: with a policy, an address it does not name gets the
// no-access page and no session. An unverified address counts for nothing.
func TestOIDCUnauthorizedEmail(t *testing.T) {
	b, f := oidcFixture(t)
	ctx := context.Background()
	if err := ApplyPolicy(ctx, b.Store, &Policy{Roles: []Role{{Name: "chat", Description: "Chat", Permissions: []string{"inference:chat", "access:manage"}}},
		Bindings: []Binding{{Kind: "email", Role: "chat", Email: "owner@example.com", Issuer: f.srv.URL}}}); err != nil {
		t.Fatal(err)
	}
	permissions := func(claims map[string]any) []string {
		t.Helper()
		f.claims = claims
		session := cookieNamed(login(t, b, f, nil), SessionCookie)
		if session == nil {
			t.Fatal("no session")
		}
		id, _ := b.Authenticate(get("/app/session", session))
		if id == nil {
			t.Fatal("no identity")
		}
		return b.Authorize(ctx, id).Permissions
	}
	if got := permissions(nil); strings.Join(got, ",") != "inference:chat,access:manage" {
		t.Errorf("the named address: %v", got)
	}
	for name, claims := range map[string]map[string]any{
		"another address":      {"sub": "user-2", "email": "someone@example.com"},
		"unverified, as owner": {"sub": "user-3", "email_verified": false},
		"no address":           {"sub": "user-4", "email": nil, "email_verified": nil},
	} {
		f.claims = claims
		resp := login(t, b, f, nil)
		if resp.StatusCode != http.StatusForbidden || cookieNamed(resp, SessionCookie) != nil {
			t.Errorf("%s: %d, session %v", name, resp.StatusCode, cookieNamed(resp, SessionCookie))
		}
	}
}

func TestOIDCSessionExpiry(t *testing.T) {
	b, f := oidcFixture(t)
	session := func() *http.Cookie {
		t.Helper()
		f.claims = nil
		c := cookieNamed(login(t, b, f, nil), SessionCookie)
		if c == nil {
			t.Fatal("no session")
		}
		return c
	}
	alive := func(c *http.Cookie) bool {
		id, _ := b.Authenticate(get("/app/session", c))
		return id != nil
	}
	rows := func() int {
		var n int
		b.Store.DB.QueryRow(`SELECT count(*) FROM auth_browser_sessions`).Scan(&n)
		return n
	}
	now := ms(time.Now())
	c := session()
	if !alive(c) {
		t.Fatal("a new session")
	}
	b.Store.DB.Exec(`UPDATE auth_browser_sessions SET idle_expires_at = ?`, now-1)
	if alive(c) {
		t.Error("an idle-expired session worked")
	}
	if rows() != 0 {
		t.Error("an expired session stayed stored")
	}
	c = session()
	b.Store.DB.Exec(`UPDATE auth_browser_sessions SET absolute_expires_at = ?`, now-1)
	if alive(c) {
		t.Error("an absolute-expired session worked")
	}
	// Use moves the idle expiry, at most hourly.
	c = session()
	b.Store.DB.Exec(`UPDATE auth_browser_sessions SET last_seen_at = ?, idle_expires_at = ?`, now-2*time.Hour.Milliseconds(), now+time.Minute.Milliseconds())
	if !alive(c) {
		t.Fatal("a live session")
	}
	var idle int64
	b.Store.DB.QueryRow(`SELECT idle_expires_at FROM auth_browser_sessions`).Scan(&idle)
	if idle < now+SessionIdleTTL.Milliseconds()-time.Minute.Milliseconds() {
		t.Errorf("idle expiry not moved: %d", idle-now)
	}
	// A session from a provider no longer configured is dropped.
	c = session()
	b.Store.DB.Exec(`UPDATE auth_browser_sessions SET issuer = 'https://gone.example.com'`)
	if alive(c) {
		t.Error("a session from a removed provider worked")
	}
	// A cookie that is not exactly one well-formed value is no session.
	c = session()
	r := httptest.NewRequest("GET", "/app/session", nil)
	r.AddCookie(c)
	r.AddCookie(c)
	if id, _ := b.Authenticate(r); id != nil {
		t.Error("two session cookies were accepted")
	}
}

// No failure writes a code, token, state or secret to the log.
func TestOIDCLogsNoCredentials(t *testing.T) {
	b, f := oidcFixture(t)
	var logs bytes.Buffer
	b.Log = slog.New(slog.NewTextHandler(&logs, nil))
	var state string
	tokens := []string{}
	f.tamper = func(token string, _ map[string]any) string {
		tokens = append(tokens, token)
		return token[:len(token)-8] + "AAAAAAAA"
	}
	refused(t, "forged", login(t, b, f, func(q url.Values, s *http.Cookie) *http.Cookie {
		state = q.Get("state")
		return s
	}))
	f.tamper = nil
	f.claims = map[string]any{"nonce": "wrong-nonce"}
	refused(t, "wrong nonce", login(t, b, f, nil))
	got := logs.String()
	if got == "" {
		t.Fatal("expected a logged failure to scan")
	}
	for _, secret := range append(tokens, "good-code", "shh", state, "wrong-nonce") {
		if secret != "" && strings.Contains(got, secret) {
			t.Errorf("the log holds %q", secret)
		}
	}
}
