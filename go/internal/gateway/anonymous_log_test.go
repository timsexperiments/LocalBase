package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/obs"
)

func anonymousLogGateway(t *testing.T, every time.Duration, burst int) (*httptest.Server, *Server, *syncBuffer, string) {
	t.Helper()
	ts, _, _ := newTestGateway(t, host{})
	srv := ts.Config.Handler.(*Server)
	logs := &syncBuffer{}
	srv.Log = slog.New(slog.NewJSONHandler(logs, nil))
	srv.NoAuth = false
	srv.anonLog = &obs.Sampler{Every: every, Burst: burst}
	_, key, err := srv.Store.CreateAPIKey(context.Background(), "read", []string{"models:read"})
	if err != nil {
		t.Fatal(err)
	}
	return ts, srv, logs, key
}

func getWith(t *testing.T, url, credential string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// Anyone can send these, so what they write to the log stays small. Someone with a key is
// always on record.
func TestRequestsWithNoCredentialAreCountedNotLogged(t *testing.T) {
	ts, _, logs, key := anonymousLogGateway(t, time.Hour, 5)
	const route = `"event":"http.request"`
	for range 300 {
		if code := getWith(t, ts.URL+"/v1/models", ""); code != 401 {
			t.Fatalf("no credential: %d", code)
		}
	}
	if n := strings.Count(logs.String(), route); n != 5 {
		t.Errorf("%d lines for 300 requests with no credential", n)
	}
	for range 50 {
		getWith(t, ts.URL+"/v1/models", "lb_notarealkeyatall")
	}
	if n := strings.Count(logs.String(), route); n != 5 {
		t.Errorf("%d lines after requests with a wrong key", n)
	}
	for range 50 {
		if code := getWith(t, ts.URL+"/v1/models", key); code != 200 {
			t.Fatalf("with a key: %d", code)
		}
	}
	if n := strings.Count(logs.String(), route); n != 55 {
		t.Errorf("%d lines: every request with a key should be logged", n)
	}
	if strings.Contains(logs.String(), "lb_notarealkeyatall") {
		t.Error("a credential reached the log")
	}
}

// What was held back is reported when the interval has passed.
func TestWhatWasHeldBackIsSummarised(t *testing.T) {
	ts, _, logs, _ := anonymousLogGateway(t, 200*time.Millisecond, 5)
	for range 50 {
		getWith(t, ts.URL+"/v1/models", "")
	}
	time.Sleep(250 * time.Millisecond)
	getWith(t, ts.URL+"/health", "") // anything arriving later brings it out, even a probe
	if got := logs.String(); !strings.Contains(got, `"event":"http.anonymous-summary"`) || !strings.Contains(got, `"count":45`) {
		t.Errorf("no summary: %s", got)
	}
}

// With --no-auth nobody is anonymous: it's one person on loopback, and every request counts.
func TestNoAuthIsNotSampled(t *testing.T) {
	ts, srv, logs, _ := anonymousLogGateway(t, time.Hour, 5)
	srv.NoAuth = true
	for range 40 {
		getWith(t, ts.URL+"/v1/models", "")
	}
	if n := strings.Count(logs.String(), `"event":"http.request"`); n != 40 {
		t.Errorf("%d lines", n)
	}
}

// A refused change by someone with no credential isn't an audit event: anyone can send it.
func TestAnonymousRefusalsAreNotAudited(t *testing.T) {
	ts, _, logs, key := anonymousLogGateway(t, time.Hour, 5)
	post := func(credential string) int {
		req, _ := http.NewRequest("POST", ts.URL+"/_localbase/api-keys", strings.NewReader(`{"action":"create","name":"x","scopes":[]}`))
		if credential != "" {
			req.Header.Set("Authorization", "Bearer "+credential)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	if code := post(""); code != 401 {
		t.Errorf("no credential: %d", code)
	}
	if code := post("lb_notarealkeyatall"); code != 401 {
		t.Errorf("a wrong key: %d", code)
	}
	if strings.Contains(logs.String(), "audit.") {
		t.Errorf("anonymous refusals were audited: %s", logs.String())
	}
	// Someone with a key who isn't allowed is.
	if code := post(key); code != 403 {
		t.Errorf("a key without the permission: %d", code)
	}
	if !strings.Contains(logs.String(), `"event":"audit.api-key-refused"`) {
		t.Errorf("not audited: %s", logs.String())
	}
}

// Something going wrong is always on record, whoever asked.
func TestServerErrorsAreAlwaysLogged(t *testing.T) {
	ts, srv, logs, _ := anonymousLogGateway(t, time.Hour, 0)
	srv.mux.HandleFunc("/test-only-fails", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	for range 10 {
		getWith(t, ts.URL+"/test-only-fails", "")
	}
	if n := strings.Count(logs.String(), `"event":"http.request"`); n != 10 {
		t.Errorf("%d lines", n)
	}
}
