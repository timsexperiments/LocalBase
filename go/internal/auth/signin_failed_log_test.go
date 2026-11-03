package auth

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Anyone can start a sign-in and fail it, so what that writes to the log stays small.
func TestFailedSignInsAreLoggedSparingly(t *testing.T) {
	st := testStore(t)
	f := newFakeIdP(t)
	reg := Registration{Kind: "oidc", ID: "idp", Name: "IdP", Issuer: f.srv.URL, ClientID: "cid",
		ClientAuthentication: &ClientAuth{Kind: "client-secret-basic", ClientSecret: "shh"}}
	b := browserFor(t, st, reg)
	b.Client = f.srv.Client()
	var logs bytes.Buffer
	b.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	for range 100 {
		login(t, b, f, func(q url.Values, s *http.Cookie) *http.Cookie {
			q.Set("code", "bad")
			return s
		})
	}
	if n := strings.Count(logs.String(), `"event":"auth.signin-failed"`); n != b.failedLog.Burst {
		t.Errorf("%d lines for 100 failures", n)
	}
}
