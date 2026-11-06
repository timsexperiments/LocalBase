package gateway

import (
	"context"
	"net/http"
	"testing"
)

// Changing a model's placement or unloading it takes models:manage and nothing else. Reading its
// placement takes models:read. A key with the other one is refused, whichever way round.
func TestModelRoutesAskForTheirOwnPermission(t *testing.T) {
	g := newMatrixGateway(t, nil)
	key := func(scope string) map[string]string {
		t.Helper()
		_, secret, err := g.store.CreateAPIKey(context.Background(), scope, []string{scope})
		if err != nil {
			t.Fatal(err)
		}
		return map[string]string{"Authorization": "Bearer " + secret, "Content-Type": "application/json"}
	}
	reader, manager := key("models:read"), key("models:manage")
	placement := "/_localbase/models/" + chatModel + "/placement"
	unload := "/_localbase/models/" + chatModel + "/unload"
	for _, c := range []struct {
		name         string
		method, path string
		body         string
		allowed      map[string]string
		answer       int // from the route itself, past the permission check
		refused      map[string]string
	}{
		{"reading a placement", http.MethodGet, placement, "", reader, http.StatusOK, manager},
		{"changing a placement", http.MethodPut, placement, `{}`, manager, http.StatusOK, reader},
		{"resetting a placement", http.MethodDelete, placement, "", manager, http.StatusOK, reader},
		{"unloading", http.MethodPost, unload, "", manager, http.StatusNotFound, reader}, // nothing is loaded
	} {
		if r := g.raw(t, c.method, c.path, "", c.refused, c.body); r.status != http.StatusForbidden {
			t.Errorf("%s with the wrong permission: %d %.200q", c.name, r.status, r.body)
		}
		if r := g.raw(t, c.method, c.path, "", c.allowed, c.body); r.status != c.answer {
			t.Errorf("%s with the right permission alone: %d %.200q, want %d", c.name, r.status, r.body, c.answer)
		}
	}
}
