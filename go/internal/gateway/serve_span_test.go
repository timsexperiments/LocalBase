package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/otlp"
)

// spanCollector is an OTLP collector for a test: what the gateway's exporter sends, as raw bytes.
type spanCollector struct {
	rt     *otlp.Runtime
	mu     sync.Mutex
	bodies [][]byte
}

func newSpanCollector(t *testing.T) *spanCollector {
	t.Helper()
	c := &spanCollector{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, b)
		c.mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	cfg, err := obs.ResolveOtel(srv.URL, "", 100, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	c.rt = otlp.New(cfg, "test", func(string, string, map[string]any) {})
	return c
}

// exported sends what is held and returns everything the collector received.
func (c *spanCollector) exported() []byte {
	c.rt.Shutdown()
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Join(c.bodies, nil)
}

func get(t *testing.T, url string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// The server span is named for the route a request reached, method first, and its http.route says
// the same (TS serve).
func TestTheServerSpanIsNamedForItsRoute(t *testing.T) {
	c := newSpanCollector(t)
	ts := newTestServerConfigured(t, func(conf *Config) { conf.Otel = c.rt })
	get(t, ts.URL+"/v1/models")
	out := c.exported()
	if !bytes.Contains(out, []byte("GET /v1/models")) {
		t.Errorf("no span named GET /v1/models")
	}
	if !bytes.Contains(out, []byte("http.route")) {
		t.Errorf("no http.route attribute")
	}
}

// A request the browser layer forwards (/app/api) renames its span after the route it reached, so
// a page's call reads as the API call it made, not as /app/api.
func TestAForwardedRequestsSpanIsNamedForTheRouteItReached(t *testing.T) {
	c := newSpanCollector(t)
	ts := newTestServerConfigured(t, func(conf *Config) { conf.Otel = c.rt }) // --no-auth: the page is let through
	get(t, ts.URL+"/app/api/_localbase/models")
	if out := c.exported(); !bytes.Contains(out, []byte("GET /_localbase/models")) {
		t.Errorf("the forwarded request's span isn't named GET /_localbase/models")
	}
}
