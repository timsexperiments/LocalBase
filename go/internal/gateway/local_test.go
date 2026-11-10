package gateway

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
)

func newLocalGateway(t *testing.T) (gatewaytest.LocalClient, *Server) {
	t.Helper()
	ts, _, _ := newTestGateway(t, host{})
	s := runServer(ts)
	s.NoAuth, s.LoginPages = false, gatewaytest.Pages{}
	return gatewaytest.LocalClient{T: t, Base: ts.URL}, s
}

// A link preview or prefetching browser cannot use a link up: GET never spends it.
func TestLoginLinkSurvivesPreview(t *testing.T) {
	c, s := newLocalGateway(t)
	token, _ := auth.IssueLocalLink(context.Background(), s.Store)
	for _, method := range []string{"GET", "GET", "GET"} {
		resp, body := c.Send(method, auth.LocalOncePath+"?token="+token, "", map[string]string{"Accept": "text/html"})
		if resp.StatusCode != 200 || body != "redeem page" {
			t.Fatalf("%s: %d %s", method, resp.StatusCode, body)
		}
		if len(resp.Cookies()) != 0 {
			t.Errorf("a GET set a cookie: %v", resp.Cookies())
		}
	}
	if resp, _ := c.Send("HEAD", auth.LocalOncePath, "", nil); resp.StatusCode != 405 {
		t.Errorf("HEAD: %d", resp.StatusCode)
	}
	if resp, _ := c.Redeem(token, nil); resp.StatusCode != 303 {
		t.Errorf("the link was used up by previews: %d", resp.StatusCode)
	}
	resp, body := c.Redeem(token, nil)
	if resp.StatusCode != 410 || body != "expired page" {
		t.Errorf("second use: %d %s", resp.StatusCode, body)
	}
}

// A forged cross-site POST, or one not sent as a form, does not redeem.
func TestLoginLinkRedeemForm(t *testing.T) {
	c, s := newLocalGateway(t)
	token, _ := auth.IssueLocalLink(context.Background(), s.Store)
	for name, h := range map[string]map[string]string{
		"other origin": {"Origin": "https://attacker.example"},
		"cross site":   {"Sec-Fetch-Site": "cross-site"},
		"not a form":   {"Content-Type": "application/json"},
		"no origin":    {"Origin": ""},
	} {
		if resp, _ := c.Redeem(token, h); resp.StatusCode != 410 {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	if resp, _ := c.Redeem(token, nil); resp.StatusCode != 303 {
		t.Errorf("the refused tries spent the link: %d", resp.StatusCode)
	}
}

// Proxied requests can neither redeem a link nor use a valid session.
func TestLocalSessionLoopbackRule(t *testing.T) {
	c, s := newLocalGateway(t)
	ck := gatewaytest.SessionCookie(t, c, s.Store)
	if resp, body := c.Send("GET", "/app/session", "", gatewaytest.LocalUI(ck, c.Base)); resp.StatusCode != 200 || !strings.Contains(body, `"localSession":true`) || !strings.Contains(body, `"system:manage"`) {
		t.Fatalf("session: %d %s", resp.StatusCode, body)
	}
	markers := []map[string]string{
		{"X-Forwarded-For": "203.0.113.9"}, {"Forwarded": "for=203.0.113.9"}, {"X-Forwarded-Host": "localbase.example.com"},
		{"X-Forwarded-Proto": "https"}, {"X-Real-Ip": "203.0.113.9"}, {"Cf-Connecting-Ip": "203.0.113.9"}, {"Cf-Ray": "abc"},
		{"Cf-Access-Jwt-Assertion": "x"}, {"Host": "localbase.example.com"}, {"Host": "attacker.example:80"}, {"Host": "127.0.0.1:1"},
	}
	for _, m := range markers {
		label := ""
		for k, v := range m {
			label = k + "=" + v
		}
		token, _ := auth.IssueLocalLink(context.Background(), s.Store)
		resp, body := c.Redeem(token, m)
		if resp.StatusCode != 403 || len(resp.Cookies()) != 0 {
			t.Errorf("redeem with %s: %d %s", label, resp.StatusCode, body)
		}
		h := gatewaytest.LocalUI(ck, c.Base)
		for k, v := range m {
			h[k] = v
		}
		if resp, body := c.Send("GET", "/app/session", "", h); resp.StatusCode == 200 {
			t.Errorf("session with %s: %d %s", label, resp.StatusCode, body)
		}
		if resp, _ := c.Send("GET", "/app/api/_localbase/models", "", h); resp.StatusCode == 200 {
			t.Errorf("API with %s: %d", label, resp.StatusCode)
		}
	}
	// The refusals left the session usable from the machine itself.
	if resp, _ := c.Send("GET", "/app/session", "", gatewaytest.LocalUI(ck, c.Base)); resp.StatusCode != 200 {
		t.Errorf("after the refusals: %d", resp.StatusCode)
	}
}

func TestLocalSessionCSRF(t *testing.T) {
	c, s := newLocalGateway(t)
	ck := gatewaytest.SessionCookie(t, c, s.Store)
	const path = "/app/api/_localbase/local-sessions"
	body := `{"action":"revoke","id":"none"}`
	for name, h := range map[string]map[string]string{
		"no marker":    {"Cookie": ck, "Origin": c.Base, "Sec-Fetch-Site": "same-origin"},
		"other origin": gatewaytest.LocalUI(ck, "https://attacker.example"),
		"cross site":   {"Cookie": ck, "X-Localbase-Ui": "1", "Origin": c.Base, "Sec-Fetch-Site": "cross-site"},
		"no origin":    {"Cookie": ck, "X-Localbase-Ui": "1", "Sec-Fetch-Site": "same-origin"},
	} {
		if resp, b := c.Send("POST", path, body, h); resp.StatusCode != 403 {
			t.Errorf("%s: %d %s", name, resp.StatusCode, b)
		}
	}
	if resp, b := c.Send("POST", path, body, gatewaytest.LocalUI(ck, c.Base)); resp.StatusCode != 200 {
		t.Errorf("same origin: %d %s", resp.StatusCode, b)
	}
	if resp, _ := c.Send("GET", path, "", map[string]string{"X-Localbase-Ui": "1"}); resp.StatusCode != 401 {
		t.Errorf("no session: %d", resp.StatusCode)
	}
}

func TestLocalSessionLogoutAndRevoke(t *testing.T) {
	c, s := newLocalGateway(t)
	ck := gatewaytest.SessionCookie(t, c, s.Store)
	resp, _ := c.Send("POST", "/app/logout", "", gatewaytest.LocalUI(ck, c.Base))
	if resp.StatusCode != 204 {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if len(resp.Cookies()) != 1 || resp.Cookies()[0].MaxAge >= 0 {
		t.Errorf("logout cookie: %v", resp.Cookies())
	}
	if resp, _ := c.Send("GET", "/app/session", "", gatewaytest.LocalUI(ck, c.Base)); resp.StatusCode == 200 {
		t.Error("the session outlived logout")
	}
	ck = gatewaytest.SessionCookie(t, c, s.Store)
	other := gatewaytest.SessionCookie(t, c, s.Store)
	resp, body := c.Send("GET", "/app/api/_localbase/local-sessions", "", gatewaytest.LocalUI(ck, c.Base))
	if resp.StatusCode != 200 || strings.Contains(body, "hash") || strings.Contains(body, strings.TrimPrefix(ck, auth.LocalCookie+"=")) {
		t.Fatalf("list: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"expiresAt"`) {
		t.Errorf("list: %s", body)
	}
	resp, body = c.Send("POST", "/app/api/_localbase/local-sessions", `{"action":"revoke-all"}`, gatewaytest.LocalUI(ck, c.Base))
	if resp.StatusCode != 200 || !strings.Contains(body, `"revoked":2`) {
		t.Fatalf("revoke all: %d %s", resp.StatusCode, body)
	}
	for _, x := range []string{ck, other} {
		if resp, _ := c.Send("GET", "/app/session", "", gatewaytest.LocalUI(x, c.Base)); resp.StatusCode == 200 {
			t.Error("a revoked session worked")
		}
	}
}

func TestLoginLinkRateLimited(t *testing.T) {
	c, s := newLocalGateway(t)
	for range 10 { // failed redeems allowed a minute

		if resp, _ := c.Redeem(auth.RandomToken(32), nil); resp.StatusCode != 410 {
			t.Fatalf("a wrong token: %d", resp.StatusCode)
		}
	}
	token, _ := auth.IssueLocalLink(context.Background(), s.Store)
	resp, body := c.Redeem(token, nil)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" || body != "too many page" {
		t.Fatalf("limited: %d %q %s", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
}

func TestLoginHelpPage(t *testing.T) {
	c, _ := newLocalGateway(t)
	resp, body := c.Send("GET", "/app/login", "", map[string]string{"Accept": "text/html,application/xhtml+xml"})
	if resp.StatusCode != 403 || body != "help local" {
		t.Errorf("navigation: %d %s", resp.StatusCode, body)
	}
	resp, body = c.Send("GET", "/app/login", "", map[string]string{"Accept": "text/html", "X-Forwarded-For": "203.0.113.9"})
	if resp.StatusCode != 403 || body != "help remote" {
		t.Errorf("proxied navigation: %d %s", resp.StatusCode, body)
	}
	resp, body = c.Send("GET", "/app/login", "", map[string]string{"Accept": "application/json"})
	if resp.StatusCode != 401 || !strings.Contains(body, "ui_access_denied") {
		t.Errorf("API client: %d %s", resp.StatusCode, body)
	}
}

// Ids only: the token, and a URL with it, never reach a log.
func TestLoginLinkAudit(t *testing.T) {
	var logs bytes.Buffer
	gatewaytest.Log = &logs
	defer func() { gatewaytest.Log = nil }()
	c, s := newLocalGateway(t)
	token, _ := auth.IssueLocalLink(context.Background(), s.Store)
	c.Redeem(token, nil)
	c.Redeem(token, nil)
	c.Redeem(token, map[string]string{"X-Forwarded-For": "203.0.113.9"})
	got := logs.String()
	for _, want := range []string{"audit.local-login-redeemed", `"link_id":"` + auth.LocalLinkID(token) + `"`, "auth.local-login-refused", `"reason":"expired"`, `"reason":"not-loopback"`} {
		if !strings.Contains(got, want) {
			t.Errorf("no %s in\n%s", want, got)
		}
	}
	if strings.Contains(got, token) {
		t.Error("a log holds the token")
	}
}

// The web path takes an id in JSON, so a leading '-' means nothing to it.
func TestRevokeASessionWhoseIDStartsWithADashOverTheWeb(t *testing.T) {
	c, s := newLocalGateway(t)
	ck := gatewaytest.SessionCookie(t, c, s.Store)
	other := gatewaytest.SessionCookie(t, c, s.Store)
	var id string
	if err := s.Store.DB.QueryRow(`SELECT id FROM local_sessions ORDER BY created_at DESC, rowid DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	const dashed = "-AnZqiOlvvn8"
	if _, err := s.Store.DB.Exec(`UPDATE local_sessions SET id = ? WHERE id = ?`, dashed, id); err != nil {
		t.Fatal(err)
	}
	resp, body := c.Send("POST", "/app/api/_localbase/local-sessions", `{"action":"revoke","id":"`+dashed+`"}`, gatewaytest.LocalUI(ck, c.Base))
	if resp.StatusCode != 200 || !strings.Contains(body, `"revoked":1`) {
		t.Fatalf("revoke: %d %s", resp.StatusCode, body)
	}
	if resp, _ := c.Send("GET", "/app/session", "", gatewaytest.LocalUI(other, c.Base)); resp.StatusCode == 200 {
		t.Error("the revoked session worked")
	}
}
