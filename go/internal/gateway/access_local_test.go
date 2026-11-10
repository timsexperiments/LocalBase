package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
)

// TestLocalSessionLoopbackRule and TestLoginLinkRateLimited (local_test.go) through Access alone,
// with what they read inside it: the principal a request gets, and the window of failed redeems.

// Proxied requests can neither redeem a link nor use a valid session.
func TestAccessLocalSessionLoopbackRule(t *testing.T) {
	ts, a, st := newAccessHarness(t)
	c := gatewaytest.LocalClient{T: t, Base: ts.URL}
	ck := gatewaytest.SessionCookie(t, c, st)
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
		token, _ := auth.IssueLocalLink(context.Background(), st)
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
		if p := a.localPrincipal(gatewaytest.MustLocalRequest(t, c, ck, m)); p != nil {
			t.Errorf("principal with %s", label)
		}
	}
	// The refusals left the session usable from the machine itself.
	if resp, _ := c.Send("GET", "/app/session", "", gatewaytest.LocalUI(ck, c.Base)); resp.StatusCode != 200 {
		t.Errorf("after the refusals: %d", resp.StatusCode)
	}
}

func TestAccessLoginLinkRateLimited(t *testing.T) {
	ts, a, st := newAccessHarness(t)
	c := gatewaytest.LocalClient{T: t, Base: ts.URL}
	for range localRedeemFailures {
		if resp, _ := c.Redeem(auth.RandomToken(32), nil); resp.StatusCode != 410 {
			t.Fatalf("a wrong token: %d", resp.StatusCode)
		}
	}
	token, _ := auth.IssueLocalLink(context.Background(), st)
	resp, body := c.Redeem(token, nil)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" || body != "too many page" {
		t.Fatalf("limited: %d %q %s", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	// A proxied visitor cannot use up the local person's tries, and the window ends.
	a.localFails.mu.Lock()
	a.localFails.start = time.Now().Add(-time.Minute)
	a.localFails.mu.Unlock()
	for range localRedeemFailures + 2 {
		c.Redeem(auth.RandomToken(32), map[string]string{"X-Forwarded-For": "203.0.113.9"})
	}
	if resp, _ := c.Redeem(token, nil); resp.StatusCode != 303 {
		t.Errorf("after the window: %d", resp.StatusCode)
	}
}
