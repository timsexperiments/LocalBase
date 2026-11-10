package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// noAuthCall sends a request with no credential and returns its status and body.
func noAuthCall(t *testing.T, ts *httptest.Server, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// Under --no-auth, a caller with no credential gets past the update and usage routes' permission
// checks, which now take the principal's word alone (TestNoAuthPrincipalsHoldEveryPermission).
// A 403 here would mean the no-auth principal lacks a permission a route asks for.
func TestNoAuthPassesTheUpdateAndUsageChecks(t *testing.T) {
	// No update check and no usage recorder: past the permission check, each says it isn't
	// configured.
	ts := newTestServer(t)
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/_localbase/update", ""},
		{"POST", "/_localbase/update", `{}`},
		{"POST", "/_localbase/usage", `{}`},
	} {
		if status, body := noAuthCall(t, ts, c.method, c.path, c.body); status != 501 {
			t.Errorf("--no-auth %s %s: %d %s, want 501 (past the permission check)", c.method, c.path, status, body)
		}
	}
	// Seeing everyone's usage needs a recorder to answer.
	withUsage := openAPIGateway(t, func(c *Config) { c.NoAuth = true })
	if status, body := noAuthCall(t, withUsage, "GET", "/_localbase/usage?scope=all", ""); status != 200 {
		t.Errorf("--no-auth GET /_localbase/usage?scope=all: %d %s, want 200", status, body)
	}
}
