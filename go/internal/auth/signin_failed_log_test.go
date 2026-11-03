package auth

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/obs"
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
	b.failedLog = &obs.Sampler{Every: time.Hour, Burst: 20} // one interval, however long this takes
	b.LogHeldBack()
	if strings.Contains(logs.String(), "auth.signin-failed-summary") {
		t.Errorf("a summary before anything failed: %s", logs.String())
	}
	for range 100 {
		login(t, b, f, func(q url.Values, s *http.Cookie) *http.Cookie {
			q.Set("code", "bad")
			return s
		})
	}
	if n := strings.Count(logs.String(), `"event":"auth.signin-failed"`); n != b.failedLog.Burst {
		t.Errorf("%d lines for 100 failures", n)
	}
	// At shutdown no failure is coming to say how many were held back.
	b.LogHeldBack()
	b.LogHeldBack()
	if n := strings.Count(logs.String(), `"event":"auth.signin-failed-summary"`); n != 1 || !strings.Contains(logs.String(), `"held_back":80`) {
		t.Errorf("%d summaries: %s", n, logs.String())
	}
}
