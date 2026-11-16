package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/otlp"
)

// A run is traced by the gateway's exporter: the run manager gets the gateway's Otel, and its
// span, localbase.run, reaches a collector.
func TestARunIsTraced(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
	}))
	defer collector.Close()
	cfg, err := obs.ResolveOtel(collector.URL, "", 100, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	rt := otlp.New(cfg, "test", func(string, string, map[string]any) {})
	ts := newTestServerConfigured(t, func(c *Config) { c.Otel = rt })
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	id := c.Start("fake:reply traced")
	c.WaitStatus(id, "completed")
	rt.Shutdown() // sends what is held
	mu.Lock()
	defer mu.Unlock()
	if !bytes.Contains(bytes.Join(bodies, nil), []byte("localbase.run")) {
		t.Errorf("no localbase.run span among %d exports", len(bodies))
	}
}
