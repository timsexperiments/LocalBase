package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

const chatRequest = `{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}`

// send is one request to a test gateway with a chosen Host and headers.
func send(t *testing.T, base, method, path, host string, headers map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, base+path, strings.NewReader(chatRequest))
	req.Header.Set("Content-Type", "application/json")
	if host != "" {
		req.Host = host
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// A --no-auth gateway refuses a foreign origin, a rebinding Host and anything
// proxied, on the routes that act with no credentials, and still answers local
// clients and its own pages.
func TestNoAuthRefusesForeignRequests(t *testing.T) {
	ts := newTestServer(t)
	self := strings.TrimPrefix(ts.URL, "http://")
	const refused = "no_auth_request_refused"
	for _, path := range []string{"/v1/chat/completions", "/app/api/v1/chat/completions", "/_localbase/runs/v1/chat/completions"} {
		for name, c := range map[string]struct {
			host    string
			headers map[string]string
		}{
			"foreign Origin":            {"", map[string]string{"Origin": "https://evil.example"}},
			"same host, other port":     {"", map[string]string{"Origin": "http://127.0.0.1:1"}},
			"null Origin":               {"", map[string]string{"Origin": "null"}},
			"cross-site fetch":          {"", map[string]string{"Sec-Fetch-Site": "cross-site"}},
			"same-site fetch":           {"", map[string]string{"Sec-Fetch-Site": "same-site"}},
			"rebinding Host":            {"evil.example", nil},
			"rebinding Host and Origin": {"evil.example", map[string]string{"Origin": "http://evil.example"}},
			"LAN Host":                  {"192.168.1.9:2273", nil},
			"tunnel Host":               {"localbase.example.org", nil},
			"X-Forwarded-For":           {"", map[string]string{"X-Forwarded-For": "203.0.113.9"}},
			"Forwarded":                 {"", map[string]string{"Forwarded": "for=203.0.113.9"}},
			"Cf-Connecting-Ip":          {"", map[string]string{"Cf-Connecting-Ip": "203.0.113.9"}},
		} {
			if code, body := send(t, ts.URL, "POST", path, c.host, c.headers); code != 403 || !strings.Contains(body, refused) {
				t.Errorf("%s %s: %d %s", path, name, code, body)
			}
		}
		// Reaches the route: an unknown model, not a refusal.
		for name, c := range map[string]struct {
			host    string
			headers map[string]string
		}{
			"a client that is not a browser": {"", nil},
			"its own page":                   {"", map[string]string{"Origin": "http://" + self, "Sec-Fetch-Site": "same-origin"}},
			"localhost":                      {"localhost:" + self[strings.LastIndex(self, ":")+1:], nil},
			"typed in the address bar":       {"", map[string]string{"Sec-Fetch-Site": "none"}},
		} {
			if code, body := send(t, ts.URL, "POST", path, c.host, c.headers); strings.Contains(body, refused) || code == 403 {
				t.Errorf("%s %s: %d %s", path, name, code, body)
			}
		}
	}
	// The Host rule covers every route; other sites may still link to a page.
	if code, _ := send(t, ts.URL, "GET", "/health", "evil.example", nil); code != 403 {
		t.Errorf("/health with a rebinding Host: %d", code)
	}
	if code, body := send(t, ts.URL, "GET", "/health", "", map[string]string{"Sec-Fetch-Site": "cross-site"}); code != 200 {
		t.Errorf("/health from another site: %d %s", code, body)
	}
}

// In-process calls (the UI's) carry no browser headers and a local Host.
func TestNoAuthAllowsInProcessCalls(t *testing.T) {
	ts := newTestServer(t)
	res := runServer(ts).Call(t.Context(), nil, "GET", "/v1/models", "", nil)
	if res.Status != 200 {
		t.Errorf("Call: %d %s", res.Status, res.Body)
	}
}

// With authentication on, none of this applies: a credential decides.
func TestAuthenticatedGatewayIgnoresTheNoAuthRules(t *testing.T) {
	ts := newTestServer(t)
	s := runServer(ts)
	s.NoAuth = false
	_, key, _ := s.Store.CreateAPIKey(t.Context(), "k", []string{"models:read"})
	code, body := send(t, ts.URL, "GET", "/v1/models", "localbase.example.org", map[string]string{
		"Authorization": "Bearer " + key, "X-Forwarded-For": "203.0.113.9"})
	if code != 200 || strings.Contains(body, "no_auth_request_refused") {
		t.Errorf("%d %s", code, body)
	}
}
