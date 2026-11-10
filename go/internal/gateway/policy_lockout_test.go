package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
)

// The API answers the plain sentence, which is what Admin shows.
func TestPolicyLockoutThroughAPI(t *testing.T) {
	ts, _, _ := newConfiguredGateway(t, host{}, func(c *Config) { c.NoAuth = false })
	s := runServer(ts)
	_, key, err := s.cfg.Store.CreateAPIKey(context.Background(), "admin", []string{"access:read", "access:manage"})
	if err != nil {
		t.Fatal(err)
	}
	policy := `{"roles":[{"name":"chat","description":"Chat","permissions":["inference:chat"]}],"bindings":[{"kind":"email","role":"chat","email":"owner@example.com"}],"defaultRole":null}`
	code, out := gatewaytest.RunClient{T: t, Base: ts.URL, Key: key}.Do("POST", "/_localbase/access-management", `{"action":"apply-policy","policy":`+policy+`,"expectedPolicyRevision":null}`)
	raw, _ := json.Marshal(out)
	if code < 400 || code >= 500 || !strings.Contains(string(raw), auth.PolicyLockoutMessage) {
		t.Errorf("%d %s", code, raw)
	}
}
