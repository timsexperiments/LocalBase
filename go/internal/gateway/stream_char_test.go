package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

// Only a run's event stream gets the no-marker rule, only for GET: the wiring around
// SameOriginStream, which an EventSource relies on.
func TestRunEventStreamRule(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	s := runServer(ts)
	s.NoAuth = false
	const origin, name = "https://lb.example.com", "lb.example.com"
	cfg := &auth.BrowserAccess{Provider: auth.Provider{Kind: "direct", Registrations: []auth.Registration{{Kind: "oidc", ID: "idp", Name: "IdP",
		Issuer: "https://idp.example.com", ClientID: "cid", ClientAuthentication: &auth.ClientAuth{Kind: "none"}}}}, Origin: origin, Permissions: auth.Permissions}
	s.Browser = auth.NewBrowser(s.Store, scratchDir(t), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	token, err := auth.CreateSession(context.Background(), s.Store, "owner", auth.Identity{Issuer: "https://idp.example.com", Subject: "u"})
	if err != nil {
		t.Fatal(err)
	}
	send := func(method, path string, set map[string]string) int {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, nil)
		req.Host = name
		req.Header.Set("Cookie", auth.SessionCookie+"="+token)
		for k, v := range set {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	const events = "/app/api/_localbase/runs/run_missing/events"
	// Past the browser guard, at the route: an unknown run. No marker needed.
	if got := send("GET", events, nil); got != 404 {
		t.Errorf("an event stream: %d", got)
	}
	for label, c := range map[string]struct {
		method, path string
		set          map[string]string
	}{
		"cross-site stream":         {"GET", events, map[string]string{"Sec-Fetch-Site": "cross-site"}},
		"another origin's stream":   {"GET", events, map[string]string{"Origin": "https://attacker.example"}},
		"another route, no marker":  {"GET", "/app/api/_localbase/runs/run_missing", nil},
		"list, no marker":           {"GET", "/app/api/_localbase/runs", nil},
		"a POST to it":              {"POST", events, map[string]string{"Origin": origin}},
		"a create path named alike": {"GET", "/app/api/_localbase/runs/v1/events", nil},
	} {
		if got := send(c.method, c.path, c.set); got != 403 {
			t.Errorf("%s: %d", label, got)
		}
	}
	if isRunEvents("/_localbase/runs/v1/chat/completions") || !isRunEvents("/_localbase/runs/x/events") {
		t.Error("isRunEvents")
	}
}
