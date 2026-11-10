package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
)

const (
	oidcOrigin = "https://lb.example.com"
	oidcHost   = "lb.example.com"
	oidcIssuer = "https://idp.example.com"
)

// oidcClient sends requests with a chosen Host over a real socket.
type oidcClient struct {
	t    *testing.T
	base string
}

func (c oidcClient) send(method, path, body string, set map[string]string) (*http.Response, string) {
	c.t.Helper()
	req, err := http.NewRequest(method, c.base+path, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	for k, v := range set {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// oidcGateway is a gateway configured as the live one is: direct browser access.
// The sign-in itself is internal/auth's TestOIDCSignIn; this covers what the gateway
// wires around a session.
func oidcGateway(t *testing.T) (oidcClient, *Server, string) {
	t.Helper()
	cfg := &auth.BrowserAccess{Provider: auth.Provider{Kind: "direct", Registrations: []auth.Registration{{Kind: "oidc", ID: "idp", Name: "IdP",
		Issuer: oidcIssuer, ClientID: "cid", ClientAuthentication: &auth.ClientAuth{Kind: "none"}}}},
		Origin: oidcOrigin, Permissions: auth.Permissions}
	ts, _, _ := newConfiguredGateway(t, host{}, func(c *Config) {
		c.NoAuth = false
		c.Browser = auth.NewBrowser(c.Store, scratchDir(t), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	})
	s := runServer(ts)
	token, err := auth.CreateSession(context.Background(), s.cfg.Store, "owner-1", auth.Identity{Issuer: oidcIssuer, Subject: "user-1", VerifiedEmail: "owner@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	return oidcClient{t, ts.URL}, s, auth.SessionCookie + "=" + token
}

func oidcUI(cookie string) map[string]string {
	return map[string]string{"Host": oidcHost, "Cookie": cookie, "X-Localbase-Ui": "1", "Origin": oidcOrigin, "Sec-Fetch-Site": "same-origin"}
}

func TestOIDCSessionThroughGateway(t *testing.T) {
	c, _, ck := oidcGateway(t)
	resp, body := c.send("GET", "/app/session", "", oidcUI(ck))
	if resp.StatusCode != 200 || !strings.Contains(body, `"verifiedEmail":"owner@example.com"`) {
		t.Errorf("session: %d %s", resp.StatusCode, body)
	}
	if resp, _ := c.send("GET", "/app/session", "", map[string]string{"Host": oidcHost, "X-Localbase-Ui": "1"}); resp.StatusCode != 401 {
		t.Errorf("no cookie: %d", resp.StatusCode)
	}
	// A browser session is not an API credential: the plain route ignores the cookie.
	if resp, _ := c.send("GET", "/_localbase/models", "", map[string]string{"Host": oidcHost, "Cookie": ck}); resp.StatusCode != 401 {
		t.Errorf("the API with only a cookie: %d", resp.StatusCode)
	}
}

// CSRF on mutations, through the routes a browser really calls.
func TestOIDCMutationsNeedSameOrigin(t *testing.T) {
	c, _, ck := oidcGateway(t)
	const path = "/app/api/_localbase/model-management"
	body := `{"modelId":"no-such-model","action":"enable"}`
	withH := func(mod func(map[string]string)) map[string]string {
		h := oidcUI(ck)
		mod(h)
		return h
	}
	for name, h := range map[string]map[string]string{
		"no UI marker":   withH(func(h map[string]string) { delete(h, "X-Localbase-Ui") }),
		"no origin":      withH(func(h map[string]string) { delete(h, "Origin") }),
		"another origin": withH(func(h map[string]string) { h["Origin"] = "https://attacker.example" }),
		"http origin":    withH(func(h map[string]string) { h["Origin"] = "http://lb.example.com" }),
		"cross site":     withH(func(h map[string]string) { h["Sec-Fetch-Site"] = "cross-site" }),
		"same site":      withH(func(h map[string]string) { h["Sec-Fetch-Site"] = "same-site" }),
		"another host":   withH(func(h map[string]string) { h["Host"] = "other.example.com" }),
	} {
		if resp, b := c.send("POST", path, body, h); resp.StatusCode != 403 || !strings.Contains(b, "ui_access_denied") {
			t.Errorf("%s: %d %s", name, resp.StatusCode, b)
		}
	}
	// The same request from the page itself reaches the route: an unknown model is
	// the route's own answer, so the guard let it through.
	if resp, b := c.send("POST", path, body, oidcUI(ck)); resp.StatusCode != 400 || !strings.Contains(b, "Unknown catalog model") {
		t.Errorf("same origin: %d %s", resp.StatusCode, b)
	}
	if resp, _ := c.send("POST", "/app/api/_localbase/not-a-route", body, oidcUI(ck)); resp.StatusCode != 404 {
		t.Errorf("a route outside the list: %d", resp.StatusCode)
	}
}

func TestOIDCLogoutThroughGateway(t *testing.T) {
	c, _, ck := oidcGateway(t)
	for name, h := range map[string]map[string]string{
		"no marker":      {"Host": oidcHost, "Cookie": ck, "Origin": oidcOrigin},
		"another origin": {"Host": oidcHost, "Cookie": ck, "X-Localbase-Ui": "1", "Origin": "https://attacker.example"},
	} {
		if resp, _ := c.send("POST", "/app/logout", "", h); resp.StatusCode != 403 {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	if resp, _ := c.send("GET", "/app/logout", "", oidcUI(ck)); resp.StatusCode != 403 {
		t.Errorf("GET: %d", resp.StatusCode)
	}
	if resp, _ := c.send("GET", "/app/session", "", oidcUI(ck)); resp.StatusCode != 200 {
		t.Fatalf("refused logouts ended the session: %d", resp.StatusCode)
	}
	resp, _ := c.send("POST", "/app/logout", "", oidcUI(ck))
	if resp.StatusCode != 204 {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if cs := resp.Cookies(); len(cs) != 1 || cs[0].Name != auth.SessionCookie || cs[0].MaxAge >= 0 {
		t.Errorf("logout cookie: %v", cs)
	}
	if resp, _ := c.send("GET", "/app/session", "", oidcUI(ck)); resp.StatusCode != 401 {
		t.Errorf("after logout: %d", resp.StatusCode)
	}
}

func TestOIDCExpiredSessionThroughGateway(t *testing.T) {
	c, s, ck := oidcGateway(t)
	s.cfg.Store.DB.Exec(`UPDATE auth_browser_sessions SET idle_expires_at = ?`, time.Now().Add(-time.Second).UnixMilli())
	if resp, _ := c.send("GET", "/app/session", "", oidcUI(ck)); resp.StatusCode != 401 {
		t.Errorf("session: %d", resp.StatusCode)
	}
	if resp, _ := c.send("GET", "/app/api/_localbase/models", "", oidcUI(ck)); resp.StatusCode != 401 {
		t.Errorf("API: %d", resp.StatusCode)
	}
}

// Signed in but not named by the policy: a session with no permissions, refused at
// every route.
func TestOIDCUnauthorizedEmailThroughGateway(t *testing.T) {
	c, s, ck := oidcGateway(t)
	if err := auth.ApplyPolicy(context.Background(), s.cfg.Store, &auth.Policy{Roles: []auth.Role{{Name: "admin", Description: "Admin", Permissions: []string{"access:manage"}}},
		Bindings: []auth.Binding{{Kind: "email", Role: "admin", Email: "someone-else@example.com"}}}); err != nil {
		t.Fatal(err)
	}
	resp, body := c.send("GET", "/app/session", "", oidcUI(ck))
	if resp.StatusCode != 200 || !strings.Contains(body, `"permissions":[]`) {
		t.Errorf("session: %d %s", resp.StatusCode, body)
	}
	if resp, b := c.send("GET", "/app/api/_localbase/models", "", oidcUI(ck)); resp.StatusCode != 403 {
		t.Errorf("read: %d %s", resp.StatusCode, b)
	}
	if resp, b := c.send("POST", "/app/api/v1/chat/completions", gatewaytest.ChatBody("hi"), oidcUI(ck)); resp.StatusCode != 403 {
		t.Errorf("chat: %d %s", resp.StatusCode, b)
	}
}
