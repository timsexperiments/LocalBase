package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Characterization of Cloudflare Access, inventory item 2: nothing exercised
// Authenticate's Cloudflare path before. It pins today's behaviour, with a fake key
// set; no real Cloudflare is involved.

const (
	cfTeam     = "team.cloudflareaccess.com"
	cfIssuer   = "https://" + cfTeam
	cfAudience = "aud-tag"
)

func cloudflareFixture(t *testing.T) (*Browser, *fakeIdP) {
	t.Helper()
	f := newFakeIdP(t)
	cfg := &BrowserAccess{Provider: Provider{Kind: "cloudflare-access", TeamDomain: cfTeam, Audience: cfAudience}, Origin: testOrigin,
		Permissions: []string{"inference:chat", "models:read"}}
	b := NewBrowser(testStore(t), t.TempDir(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if want := "https://" + cfTeam + "/cdn-cgi/access/certs"; b.cfJWKS.URL != want {
		t.Errorf("the key set URL: %s", b.cfJWKS.URL)
	}
	b.cfJWKS = &JWKS{URL: f.srv.URL + "/jwks", Client: f.srv.Client()}
	return b, f
}

func cfClaims(overrides map[string]any) map[string]any {
	claims := map[string]any{"iss": cfIssuer, "aud": []any{cfAudience}, "sub": "user-1", "email": "owner@example.com", "type": "app",
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
	for k, v := range overrides {
		if v == nil {
			delete(claims, k)
		} else {
			claims[k] = v
		}
	}
	return claims
}

func cfRequest(token string) *http.Request {
	r := httptest.NewRequest("GET", "/app/session", nil)
	if token != "" {
		r.Header.Set("Cf-Access-Jwt-Assertion", token)
	}
	return r
}

func TestCloudflareGoodAssertion(t *testing.T) {
	b, f := cloudflareFixture(t)
	id, owner := b.Authenticate(cfRequest(f.sign(cfClaims(nil))))
	if id == nil || id.Issuer != cfIssuer || id.Subject != "user-1" || id.VerifiedEmail != "owner@example.com" {
		t.Fatalf("identity: %+v", id)
	}
	if owner != ownerID("cloudflare-access", cfIssuer, "user-1") {
		t.Errorf("owner: %s", owner)
	}
	if got := b.Authorize(context.Background(), id).Permissions; strings.Join(got, ",") != "inference:chat,models:read" {
		t.Errorf("permissions: %v", got)
	}
	// As in TS, any ASCII subject up to 255 characters, control characters included.
	if id, _ := b.Authenticate(cfRequest(f.sign(cfClaims(map[string]any{"sub": "user\n1"})))); id == nil {
		t.Error("an ASCII subject with a control character")
	}
	if id, _ := b.Authenticate(cfRequest(f.sign(cfClaims(map[string]any{"aud": cfAudience})))); id == nil {
		t.Error("a single-string audience should do")
	}
	// The owner follows the subject, not the address.
	_, other := b.Authenticate(cfRequest(f.sign(cfClaims(map[string]any{"email": "new@example.com"}))))
	if other != owner {
		t.Errorf("an address change moved the owner: %s", other)
	}
}

func TestCloudflareRefusals(t *testing.T) {
	b, f := cloudflareFixture(t)
	for name, claims := range map[string]map[string]any{
		"wrong audience":        {"aud": []any{"other"}},
		"wrong issuer":          {"iss": "https://other.cloudflareaccess.com"},
		"expired":               {"exp": time.Now().Add(-time.Minute).Unix()},
		"no expiry":             {"exp": nil},
		"no subject":            {"sub": nil},
		"no email":              {"email": nil},
		"invalid email":         {"email": "not-an-address"},
		"non-identity":          {"email": "non_identity@example.com"},
		"non-identity, cased":   {"email": "Non_Identity@example.com"},
		"not an app token":      {"type": "org"},
		"no type":               {"type": nil},
		"service token, cn":     {"common_name": "svc.access"},
		"service token, id":     {"service_token_id": "abc"},
		"service token, status": {"service_token_status": true},
		"non-ASCII subject":     {"sub": "usér"},
		"overlong subject":      {"sub": strings.Repeat("s", 256)},
	} {
		if id, owner := b.Authenticate(cfRequest(f.sign(cfClaims(claims)))); id != nil || owner != "" {
			t.Errorf("%s: %+v %s", name, id, owner)
		}
	}
	if id, _ := b.Authenticate(cfRequest("")); id != nil {
		t.Error("no assertion")
	}
	if id, _ := b.Authenticate(cfRequest("garbage")); id != nil {
		t.Error("garbage")
	}
	good := f.sign(cfClaims(nil))
	if id, _ := b.Authenticate(cfRequest(good[:len(good)-6] + "AAAAAA")); id != nil {
		t.Error("a corrupt signature")
	}
	enc := base64.RawURLEncoding
	seg := func(v any) string {
		raw, _ := json.Marshal(v)
		return enc.EncodeToString(raw)
	}
	if id, _ := b.Authenticate(cfRequest(seg(map[string]any{"alg": "none"}) + "." + seg(cfClaims(nil)) + ".")); id != nil {
		t.Error("alg none")
	}
	// Only RS256 is accepted here, even from the right key set.
	if id, _ := b.Authenticate(cfRequest(seg(map[string]any{"alg": "RS512", "kid": "k1"}) + "." + seg(cfClaims(nil)) + "." + enc.EncodeToString([]byte("x")))); id != nil {
		t.Error("another algorithm")
	}
}

// A session cookie means nothing here, and sign-out is Cloudflare's.
func TestCloudflareHasNoLocalSessions(t *testing.T) {
	b, _ := cloudflareFixture(t)
	token, err := CreateSession(context.Background(), b.Store, "someone", Identity{Issuer: cfIssuer, Subject: "user-1"})
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: SessionCookie, Value: token}
	r := cfRequest("")
	r.AddCookie(cookie)
	if id, _ := b.Authenticate(r); id != nil {
		t.Error("a session cookie authenticated under Cloudflare Access")
	}
	if b.Direct() {
		t.Error("Cloudflare Access is not direct")
	}
	if got := b.CloudflareLogout(); got != "/cdn-cgi/access/logout" {
		t.Errorf("logout URL: %s", got)
	}
	w := httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/app/logout", nil)
	r.AddCookie(cookie)
	b.Logout(w, r)
	if w.Code != 204 || len(w.Result().Cookies()) != 0 {
		t.Errorf("logout: %d %v", w.Code, w.Result().Cookies())
	}
	var n int
	b.Store.DB.QueryRow(`SELECT count(*) FROM auth_browser_sessions`).Scan(&n)
	if n != 1 {
		t.Errorf("logout changed stored sessions: %d", n)
	}
}

// With a policy, the verified address decides, as with OIDC.
func TestCloudflarePolicy(t *testing.T) {
	b, f := cloudflareFixture(t)
	ctx := context.Background()
	if err := ApplyPolicy(ctx, b.Store, &Policy{Roles: []Role{{Name: "admin", Description: "Admin", Permissions: []string{"access:manage"}}},
		Bindings: []Binding{{Kind: "email", Role: "admin", Email: "owner@example.com"}}}); err != nil {
		t.Fatal(err)
	}
	id, _ := b.Authenticate(cfRequest(f.sign(cfClaims(nil))))
	if got := b.Authorize(ctx, id).Permissions; strings.Join(got, ",") != "access:manage" {
		t.Errorf("the named address: %v", got)
	}
	id, _ = b.Authenticate(cfRequest(f.sign(cfClaims(map[string]any{"sub": "user-2", "email": "someone@example.com"}))))
	if id == nil {
		t.Fatal("an unnamed address is still an identity")
	}
	if got := b.Authorize(ctx, id).Permissions; len(got) != 0 {
		t.Errorf("an unnamed address: %v", got)
	}
}
