package gateway

import (
	"net/http"
	"strings"
	"testing"
)

// Each thing done to a model needs its own permission and no other (DESIGN §20.2, §23).
func TestModelRoutePermissions(t *testing.T) {
	ts := openAPIGateway(t)
	s := runServer(ts)
	key := func(scopes ...string) string {
		t.Helper()
		_, k, err := s.Store.CreateAPIKey(t.Context(), strings.Join(scopes, "+"), scopes)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	manage, read, chat := key("models:manage"), key("models:read"), key("inference:chat")
	status := func(k, method, path, body string) int {
		t.Helper()
		req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+k)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	model := "/_localbase/models/" + chatModel
	for _, c := range []struct {
		method, path, body string
		manage, read       bool
	}{
		{"POST", model + "/unload", "", true, false},
		{"PUT", model + "/placement", `{"mode":"auto"}`, true, false},
		{"DELETE", model + "/placement", "", true, false},
		{"GET", model + "/placement", "", false, true},
		{"GET", model + "/admission", "", false, true},
		{"GET", model, "", false, true},
		// Asking the wrong way is still a read.
		{"GET", model + "/unload", "", false, true},
		{"POST", model + "/placement", "", false, true},
	} {
		for _, k := range []struct {
			name, key string
			allowed   bool
		}{{"models:manage", manage, c.manage}, {"models:read", read, c.read}, {"inference:chat", chat, false}} {
			got := status(k.key, c.method, c.path, c.body)
			if k.allowed && got == 403 {
				t.Errorf("%s %s with %s: 403", c.method, c.path, k.name)
			}
			if !k.allowed && got != 403 {
				t.Errorf("%s %s with %s: %d, want 403", c.method, c.path, k.name, got)
			}
		}
	}
	// A key that may not do this cannot learn which models exist from the answer.
	for _, method := range []string{"PUT", "DELETE"} {
		if got := status(read, method, "/_localbase/models/no-such-model/placement", `{"mode":"auto"}`); got != 403 {
			t.Errorf("%s of an unknown model's placement with models:read: %d, want 403", method, got)
		}
	}
}
