package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

// Under --no-auth both principals hold every permission: the gateway's, for an API caller with no
// credential, and the browser layer's, for the page. So the principal alone decides what a caller
// may do, and a route needs no --no-auth check of its own beside it.
func TestNoAuthPrincipalsHoldEveryPermission(t *testing.T) {
	ts := newTestServer(t) // --no-auth
	p := runServer(ts).principal(httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if !slices.Equal(p.Permissions, auth.Permissions) {
		t.Errorf("the gateway's no-auth principal holds %q, want every permission %q", p.Permissions, auth.Permissions)
	}
	resp, err := http.Get(ts.URL + "/app/session")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var session struct {
		Authenticated bool     `json:"authenticated"`
		Permissions   []string `json:"permissions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !session.Authenticated || !slices.Equal(session.Permissions, auth.Permissions) {
		t.Errorf("the page's no-auth principal: %d %v holds %q, want every permission %q",
			resp.StatusCode, session.Authenticated, session.Permissions, auth.Permissions)
	}
}
