package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
)

// When a credential can't be looked up, nothing about it is known. On every route and method, a
// request with a good key is never told the key is bad or lacks permission: a client may throw away
// a key it is told is invalid.
func TestALookupFailureIsNeverAnAnswerAboutTheKey(t *testing.T) {
	g := newMatrixGateway(t, nil)
	headers := map[string]string{"Authorization": "Bearer " + g.noScopes}
	var sawGuard, sawPrincipal, sawUnmatched bool
	// Closing the store makes every key lookup fail, the way a database that won't answer does.
	if err := g.store.Close(); err != nil {
		t.Fatal(err)
	}
	for path := range matrixPaths(gatewayPatterns(t)) {
		if isPublicPath(path) || isSignInPath(path) || slices.Contains(tokenRoutes, path) {
			continue
		}
		for _, method := range matrixMethods {
			if method == http.MethodOptions {
				continue // answered before any credential is read
			}
			r := g.send(t, method, path, "", headers)
			if r.status == 401 || r.status == 403 || strings.Contains(r.body, "invalid_api_key") || strings.Contains(r.body, "insufficient_permissions") {
				t.Errorf("%s %s: %d %.200q", method, path, r.status, r.body)
			}
			switch {
			case path == "/v1/chat/completions" && method == http.MethodPost:
				sawGuard = r.status == 503
			case path == "/_localbase/model-management" && method == http.MethodPost:
				sawPrincipal = r.status == 503
			case path == "/_localbase/api-keys" && method == http.MethodPost && r.status != 503:
				t.Errorf("%s %s: %d %.200q, want 503", method, path, r.status, r.body)
			}
		}
	}
	r := g.send(t, http.MethodGet, "/v1/no-such-route", "", headers)
	sawUnmatched = r.status == 503
	if !sawGuard || !sawPrincipal || !sawUnmatched {
		t.Errorf("503s: guard %v, principal %v, unmatched %v (%d)", sawGuard, sawPrincipal, sawUnmatched, r.status)
	}
}

// The same answer carries what a client needs to come back.
func TestALookupFailureSaysWhenToRetry(t *testing.T) {
	g := newMatrixGateway(t, nil)
	if err := g.store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/_localbase/model-management", "/v1/chat/completions", "/_localbase/api-keys"} {
		req, _ := http.NewRequest(http.MethodPost, g.ts.URL+target, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+g.noScopes)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
			t.Errorf("%s: %d, Retry-After %q", target, resp.StatusCode, resp.Header.Get("Retry-After"))
		}
	}
}

// A key that really isn't one still hears so, with the database working.
func TestAKeyThatIsNotValidIsStill401(t *testing.T) {
	g := newMatrixGateway(t, nil)
	for _, target := range []string{"/_localbase/model-management", "/v1/chat/completions", "/_localbase/api-keys", "/v1/no-such-route"} {
		r := g.send(t, http.MethodPost, target, "", map[string]string{"Authorization": "Bearer lb_not_a_key_at_all"})
		if r.status != 401 {
			t.Errorf("%s: %d %.200q", target, r.status, r.body)
		}
	}
	if r := g.send(t, http.MethodPost, "/_localbase/model-management", "", map[string]string{"Authorization": "Bearer " + g.noScopes}); r.status != 403 {
		t.Errorf("a key with no permissions: %d", r.status)
	}
}

// A route can look a key up twice, and the second look can be the one that fails, after a guard
// passed it: then the principal it has holds no permissions, and that isn't a refusal either.
func TestAPermissionCheckAfterAFailedLookupIsNotARefusal(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	ctx := auth.WithLookupState(r.Context())
	auth.MarkLookupFailed(ctx)
	httpx.Forbidden(rec, r.WithContext(ctx))
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
		t.Errorf("%d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	rec = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodPost, "/", nil)
	httpx.Forbidden(rec, r.WithContext(auth.WithLookupState(r.Context())))
	if rec.Code != 403 {
		t.Errorf("a real refusal: %d", rec.Code)
	}
}

// A signed-in person whose session can't be read isn't signed out: not sent to sign in, not told no.
// Every page and browser route, with each kind of session cookie.
func TestASessionLookupFailureIsNotSignedOut(t *testing.T) {
	for _, c := range []struct {
		name   string
		access *auth.BrowserAccess
		host   string
		cookie string
	}{
		{"a browser session", directAccess(), "lb.example", auth.SessionCookie + "=" + strings.Repeat("a", 43)},
		{"a local session", nil, "", auth.LocalSessionCookie(strings.Repeat("a", 43)).String()},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newMatrixGateway(t, c.access)
			if err := g.store.Close(); err != nil {
				t.Fatal(err)
			}
			origin := "https://lb.example"
			if c.host == "" {
				origin = "http://" + g.ts.Listener.Addr().String()
			}
			headers := map[string]string{"Cookie": strings.SplitN(c.cookie, ";", 2)[0], "Origin": origin, "Sec-Fetch-Site": "same-origin", "X-Localbase-Ui": "1", "Accept": "text/html"}
			saw503 := 0
			paths := matrixPaths(uiPatterns(t))
			for _, p := range []string{"/app/session", "/app/api/_localbase/host", "/app/api/_localbase/models"} {
				paths[p] = p
			}
			for path := range paths {
				switch {
				case path == "/app/session", strings.HasPrefix(path, "/app/api/"):
				case !strings.HasPrefix(path, "/app/") || strings.HasPrefix(path, "/app/assets/"), strings.HasPrefix(path, "/app/login"), path == "/app/logout":
					continue // no session in it, or how one is made
				}
				r := g.send(t, http.MethodGet, path, c.host, headers)
				if r.status == 401 || r.status == 403 || strings.Contains(r.location, "/app/login") || strings.Contains(r.body, `href="/app/login`) {
					t.Errorf("GET %s: %d, location %q, %.160q", path, r.status, r.location, r.body)
				}
				if r.status == 503 {
					saw503++
				}
				if r.status == 503 && strings.HasPrefix(path, "/app/chat") && (!strings.Contains(r.body, "We can’t check your sign-in right now") || !strings.Contains(r.body, "<title>Can’t check your sign-in · LocalBase</title>")) {
					t.Errorf("GET %s: %.300q", path, r.body)
				}
			}
			if saw503 < 3 {
				t.Errorf("only %d answers of 503 from %d paths: the sweep isn't reaching the check", saw503, len(paths))
			}
		})
	}
}

// A page's fragment request gets no redirect either, and says when to come back.
func TestASessionLookupFailureOnAFragment(t *testing.T) {
	g := newMatrixGateway(t, directAccess())
	if err := g.store.Close(); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, g.ts.URL+"/app/chat", nil)
	req.Host = "lb.example"
	req.Header.Set("Cookie", auth.SessionCookie+"="+strings.Repeat("a", 43))
	req.Header.Set("HX-Request", "true")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" || resp.Header.Get("HX-Redirect") != "" {
		t.Errorf("%d, Retry-After %q, HX-Redirect %q", resp.StatusCode, resp.Header.Get("Retry-After"), resp.Header.Get("HX-Redirect"))
	}
}

// With the database working, a session that doesn't exist is sent to sign in, as before.
func TestASessionThatDoesNotExistIsStillSignedOut(t *testing.T) {
	g := newMatrixGateway(t, directAccess())
	headers := map[string]string{"Cookie": auth.SessionCookie + "=" + strings.Repeat("b", 43), "Accept": "text/html"}
	r := g.send(t, http.MethodGet, "/app/chat", "lb.example", headers)
	if r.status != 303 || !strings.Contains(r.location, "/app/login") {
		t.Errorf("a page: %d, location %q", r.status, r.location)
	}
	r = g.send(t, http.MethodGet, "/app/session", "lb.example", map[string]string{"Cookie": headers["Cookie"], "Origin": "https://lb.example", "Sec-Fetch-Site": "same-origin", "X-Localbase-Ui": "1"})
	if r.status != 401 {
		t.Errorf("the session route: %d", r.status)
	}
}

// A session that reads fine and a policy that doesn't: that's not a person with no permissions.
func TestAPolicyLookupFailureIsNotARefusal(t *testing.T) {
	access := directAccess()
	g := newMatrixGateway(t, access)
	ctx := context.Background()
	var issuer string
	for _, reg := range access.Provider.Registrations {
		if reg.Kind == "oidc" {
			issuer = reg.Issuer
		}
	}
	token, err := auth.CreateSession(ctx, g.store, "owner", auth.Identity{Issuer: issuer, Subject: "p", VerifiedEmail: "person@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.store.DB.ExecContext(ctx, `DROP TABLE auth_settings`); err != nil {
		t.Fatal(err)
	}
	r := g.send(t, http.MethodGet, "/app/session", "lb.example", map[string]string{"Cookie": auth.SessionCookie + "=" + token,
		"Origin": "https://lb.example", "Sec-Fetch-Site": "same-origin", "X-Localbase-Ui": "1"})
	if r.status != 503 {
		t.Errorf("the session route: %d %.200q", r.status, r.body)
	}
	r = g.send(t, http.MethodGet, "/app/chat", "lb.example", map[string]string{"Cookie": auth.SessionCookie + "=" + token, "Accept": "text/html"})
	if r.status != 503 {
		t.Errorf("a page: %d %.200q", r.status, r.body)
	}
}

// Signing out when the session can't be ended: it is still good, so the person isn't told it's gone
// and keeps the cookie.
func TestASignOutThatFailedDoesNotSaySignedOut(t *testing.T) {
	t.Run("a browser session", func(t *testing.T) {
		g := newMatrixGateway(t, directAccess())
		if err := g.store.Close(); err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodPost, g.ts.URL+"/app/logout", nil)
		req.Host = "lb.example"
		req.Header.Set("Cookie", auth.SessionCookie+"="+strings.Repeat("a", 43))
		req.Header.Set("Origin", "https://lb.example")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("X-Localbase-Ui", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" || len(resp.Cookies()) != 0 {
			t.Errorf("%d, Retry-After %q, cookies %v", resp.StatusCode, resp.Header.Get("Retry-After"), resp.Cookies())
		}
	})
	t.Run("a local session", func(t *testing.T) {
		c, s := newLocalGateway(t)
		cookie := SessionCookie(t, c, s.Store)
		if err := s.Store.Close(); err != nil {
			t.Fatal(err)
		}
		resp, _ := c.Send("POST", "/app/logout", "", LocalUI(cookie, c.Base))
		if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" || len(resp.Cookies()) != 0 {
			t.Errorf("%d, Retry-After %q, cookies %v", resp.StatusCode, resp.Header.Get("Retry-After"), resp.Cookies())
		}
	})
	t.Run("with the database working, it's gone", func(t *testing.T) {
		c, s := newLocalGateway(t)
		cookie := SessionCookie(t, c, s.Store)
		resp, _ := c.Send("POST", "/app/logout", "", LocalUI(cookie, c.Base))
		if resp.StatusCode != 204 || len(resp.Cookies()) != 1 || resp.Cookies()[0].MaxAge >= 0 {
			t.Errorf("%d, cookies %v", resp.StatusCode, resp.Cookies())
		}
	})
}

// A local sign-in link that can't be read isn't called expired, and isn't a bad try.
func TestALocalLinkThatCannotBeReadIsNotExpired(t *testing.T) {
	c, s := newLocalGateway(t)
	token, err := auth.IssueLocalLink(context.Background(), s.Store)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ { // more than the limit on bad tries
		resp, body := c.Redeem(token, nil)
		if resp.StatusCode != 503 || body != "busy page" || resp.Header.Get("Retry-After") != "5" || len(resp.Cookies()) != 0 {
			t.Fatalf("try %d: %d %q, Retry-After %q, cookies %v", i, resp.StatusCode, body, resp.Header.Get("Retry-After"), resp.Cookies())
		}
	}
}
