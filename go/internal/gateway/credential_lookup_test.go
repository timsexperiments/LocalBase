package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
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
	r = r.WithContext(context.WithValue(r.Context(), recKey{}, &recorder{ResponseWriter: rec, lookupFailed: true}))
	forbidden(rec, r)
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
		t.Errorf("%d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	rec = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodPost, "/", nil)
	r = r.WithContext(context.WithValue(r.Context(), recKey{}, &recorder{ResponseWriter: rec}))
	forbidden(rec, r)
	if rec.Code != 403 {
		t.Errorf("a real refusal: %d", rec.Code)
	}
}
