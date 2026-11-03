package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
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

// What was held back is reported when the interval has passed, however long the requests took:
// each one is either on a line of its own or in a summary.
func TestWhatWasHeldBackIsSummarised(t *testing.T) {
	ts, _, logs, _ := anonymousLogGateway(t, 200*time.Millisecond, 5)
	for range 50 {
		getWith(t, ts.URL+"/v1/models", "")
	}
	time.Sleep(250 * time.Millisecond)
	getWith(t, ts.URL+"/health", "") // anything arriving later brings it out, even a probe
	lines, summaries := anonymousEvents(t, logs)
	if len(summaries) == 0 {
		t.Fatalf("no summary: %s", logs.String())
	}
	if held := heldBack(summaries); lines+held != 50 {
		t.Errorf("%d lines and %d in summaries, for 50 requests", lines, held)
	}
}

// At shutdown no request is coming to bring it out, so that writes it, with what the open
// interval held back and the totals since start.
func TestShutdownSaysWhatWasHeldBack(t *testing.T) {
	ts, srv, logs, key := anonymousLogGateway(t, time.Hour, 5)
	for range 300 {
		getWith(t, ts.URL+"/v1/models", "")
	}
	for range 7 {
		postKeys(t, ts.URL, "")
	}
	postKeys(t, ts.URL, key) // someone with a key isn't in these counts: they're audited
	if _, summaries := anonymousEvents(t, logs); len(summaries) != 0 {
		t.Fatalf("a summary before the interval ended: %v", summaries)
	}
	srv.LogHeldBack()
	lines, summaries := anonymousEvents(t, logs)
	if len(summaries) != 1 {
		t.Fatalf("%d summaries: %s", len(summaries), logs.String())
	}
	got := summaries[0]
	if lines != 5 || got["count"] != float64(302) {
		t.Errorf("%d lines, summary %v", lines, got)
	}
	if got["refused_total"] != float64(307) || got["admin_refused_total"] != float64(7) || got["level"] != "WARN" {
		t.Errorf("totals: %v", got)
	}
	// Writing it again adds nothing to what was held back, and the totals stand.
	srv.LogHeldBack()
	_, summaries = anonymousEvents(t, logs)
	if len(summaries) != 2 || summaries[1]["count"] != float64(0) || summaries[1]["refused_total"] != float64(307) || summaries[1]["level"] != "INFO" {
		t.Errorf("after a second call: %v", summaries)
	}
}

// The totals are there in the summary written during the run as well.
func TestSummariesCarryTheTotals(t *testing.T) {
	ts, _, logs, _ := anonymousLogGateway(t, 200*time.Millisecond, 1)
	for range 10 {
		getWith(t, ts.URL+"/v1/models", "")
	}
	postKeys(t, ts.URL, "")
	time.Sleep(250 * time.Millisecond)
	getWith(t, ts.URL+"/health", "")
	_, summaries := anonymousEvents(t, logs)
	if len(summaries) == 0 {
		t.Fatalf("no summary: %s", logs.String())
	}
	last := summaries[len(summaries)-1]
	if last["refused_total"] != float64(11) || last["admin_refused_total"] != float64(1) {
		t.Errorf("totals: %v", last)
	}
}

// Only a refusal is in the totals: other answers with no credential can be held back, and aren't
// counted as refused.
func TestOnlyRefusalsAreInTheTotals(t *testing.T) {
	ts, srv, logs, _ := anonymousLogGateway(t, time.Hour, 5)
	srv.mux.HandleFunc("/test-only-missing", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })
	srv.mux.HandleFunc("/test-only-forbidden", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(403) })
	for range 20 {
		if code := getWith(t, ts.URL+"/test-only-missing", ""); code != 404 {
			t.Fatalf("answer: %d", code)
		}
	}
	for range 3 {
		if code := getWith(t, ts.URL+"/test-only-forbidden", ""); code != 403 {
			t.Fatalf("answer: %d", code)
		}
	}
	srv.LogHeldBack()
	_, summaries := anonymousEvents(t, logs)
	if len(summaries) != 1 || summaries[0]["count"] != float64(18) || summaries[0]["refused_total"] != float64(3) {
		t.Errorf("summaries: %v", summaries)
	}
}

// A change refused some other way isn't a 401 or a 403, and it's still reported.
func TestARefusedChangeAloneIsReported(t *testing.T) {
	ts, srv, logs, _ := anonymousLogGateway(t, time.Hour, 5)
	srv.mux.HandleFunc("/test-only-audited", srv.audited("api-key", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(400) }))
	resp, err := http.Post(ts.URL+"/test-only-audited", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	srv.LogHeldBack()
	_, summaries := anonymousEvents(t, logs)
	if len(summaries) != 1 || summaries[0]["admin_refused_total"] != float64(1) || summaries[0]["refused_total"] != float64(0) {
		t.Errorf("summaries: %v", summaries)
	}
}

// A gateway nobody was refused by has nothing to say when it stops.
func TestAQuietGatewaySaysNothingWhenItStops(t *testing.T) {
	ts, srv, logs, key := anonymousLogGateway(t, time.Hour, 5)
	for range 20 {
		getWith(t, ts.URL+"/health", "") // answered, and not logged
		getWith(t, ts.URL+"/v1/models", key)
	}
	srv.LogHeldBack()
	if _, summaries := anonymousEvents(t, logs); len(summaries) != 0 {
		t.Errorf("a summary with nothing to report: %v", summaries)
	}
}

// With --no-auth nothing is refused for want of a credential.
func TestNoAuthHasNoSummary(t *testing.T) {
	ts, srv, logs, _ := anonymousLogGateway(t, time.Hour, 5)
	srv.NoAuth = true
	for range 40 {
		getWith(t, ts.URL+"/v1/models", "")
		postKeys(t, ts.URL, "") // answered or refused, this is the one person on loopback
	}
	srv.LogHeldBack()
	if _, summaries := anonymousEvents(t, logs); len(summaries) != 0 {
		t.Errorf("a summary under --no-auth: %v", summaries)
	}
}

// refusingProvider publishes its endpoints and refuses every code. Nothing here leaves the process.
type refusingProvider struct{}

func (refusingProvider) RoundTrip(r *http.Request) (*http.Response, error) {
	status, body := 400, `{"error":"invalid_grant"}`
	if strings.HasSuffix(r.URL.Path, "/.well-known/openid-configuration") {
		status, body = 200, `{"issuer":"`+oidcIssuer+`","authorization_endpoint":"`+oidcIssuer+`/authorize","token_endpoint":"`+oidcIssuer+`/token",`+
			`"jwks_uri":"`+oidcIssuer+`/jwks","response_types_supported":["code"],"code_challenge_methods_supported":["S256"],`+
			`"id_token_signing_alg_values_supported":["RS256"],"token_endpoint_auth_methods_supported":["none"]}`
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

// Failed sign-ins held back are said at shutdown through the same call, and each failure is on a
// line of its own or in a count.
func TestShutdownSaysWhatFailedSignInsHeldBack(t *testing.T) {
	c, srv, _ := oidcGateway(t)
	logs := &syncBuffer{}
	srv.Browser.Log = slog.New(slog.NewJSONHandler(logs, nil))
	srv.Browser.Client = &http.Client{Transport: refusingProvider{}}
	// Both answers redirect to places that don't exist: they're read here, not followed.
	direct := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	send := func(path, cookie string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("GET", c.base+path, nil)
		req.Host = oidcHost
		if cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
		resp, err := direct.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	const failures = 25
	started := time.Now()
	for range failures {
		start := send("/app/login?provider=idp", "")
		to, err := url.Parse(start.Header.Get("Location"))
		if err != nil || to.Query().Get("state") == "" || len(start.Cookies()) != 1 {
			t.Fatalf("start: %d %s", start.StatusCode, start.Header.Get("Location"))
		}
		cookie := start.Cookies()[0]
		send(auth.OIDCCallbackPath+"?code=refused&state="+url.QueryEscape(to.Query().Get("state")), cookie.Name+"="+cookie.Value)
	}
	withinAMinute := time.Since(started) < time.Minute
	srv.LogHeldBack()
	lines, held, summaries := 0, 0, 0
	for _, line := range strings.Split(logs.String(), "\n") {
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		n, _ := ev["held_back"].(float64)
		switch ev["event"] {
		case "auth.signin-failed":
			lines++
			held += int(n)
		case "auth.signin-failed-summary":
			summaries++
			held += int(n)
		}
	}
	if lines+held != failures {
		t.Errorf("%d lines and %d held back, for %d failures: %s", lines, held, failures, logs.String())
	}
	if withinAMinute && summaries != 1 {
		t.Errorf("%d summaries: %s", summaries, logs.String())
	}
}

func postKeys(t *testing.T, base, credential string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", base+"/_localbase/api-keys", strings.NewReader(`{"action":"create","name":"x","scopes":["models:read"]}`))
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

// anonymousEvents counts the request lines with no accepted credential and returns the summaries.
func anonymousEvents(t *testing.T, logs *syncBuffer) (lines int, summaries []map[string]any) {
	t.Helper()
	for _, line := range strings.Split(logs.String(), "\n") {
		if line == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("%v: %s", err, line)
		}
		switch ev["event"] {
		case "http.anonymous-summary":
			summaries = append(summaries, ev)
		case "http.request":
			if ev["auth_outcome"] != "authenticated" {
				lines++
			}
		}
	}
	return lines, summaries
}

func heldBack(summaries []map[string]any) int {
	n := 0
	for _, s := range summaries {
		c, _ := s["count"].(float64)
		n += int(c)
	}
	return n
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
