package gateway

import (
	"errors"
	"io"
	"net/http"
	"regexp"
	"testing"
)

// requestIDPattern is what newRequestID makes: lbreq_ and a version-4 UUID.
var requestIDPattern = regexp.MustCompile(`^lbreq_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// Every answer carries X-Localbase-Request-Id, the one a caller quotes when something went wrong:
// a normal answer, the refusals made before any route runs (OPTIONS, and --no-auth's), and a
// panic's 503.
func TestEveryAnswerCarriesItsRequestID(t *testing.T) {
	ts := newTestServer(t) // --no-auth
	runServer(ts).mux.HandleFunc("GET /test/panic-for-id", func(http.ResponseWriter, *http.Request) { panic(errors.New("boom")) })
	for _, c := range []struct {
		name, method, path, host string
		status                   int
	}{
		{"an answer", "GET", "/v1/models", "", 200},
		{"OPTIONS refused before any route", "OPTIONS", "/v1/models", "", 403},
		{"--no-auth refusing a foreign Host", "GET", "/v1/models", "localbase.example.org", 403},
		{"a panic's 503", "GET", "/test/panic-for-id", "", 503},
	} {
		req, _ := http.NewRequest(c.method, ts.URL+c.path, nil)
		if c.host != "" {
			req.Host = c.host
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		id := resp.Header.Get("X-Localbase-Request-Id")
		if resp.StatusCode != c.status || !requestIDPattern.MatchString(id) {
			t.Errorf("%s: %d with request id %q, want %d and an id", c.name, resp.StatusCode, id, c.status)
		}
	}
}
