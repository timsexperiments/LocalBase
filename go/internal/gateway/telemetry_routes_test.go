package gateway

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/obs"
)

// Every route the gateway serves has a name in the telemetry, or its requests are all counted as
// "unmatched-route" and nobody can tell them apart. These are the routes the mux itself recorded, so a
// new one is covered, wherever it is registered.
func TestEveryRouteHasATelemetryName(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	routes := runServer(ts).routes
	if len(routes) < 30 {
		t.Fatalf("only %d routes recorded", len(routes))
	}
	var unnamed []string
	for _, pattern := range routes {
		_, path, found := strings.Cut(pattern, " ")
		if !found {
			path = pattern
		}
		// As routeLabel makes it from the request's pattern.
		path = strings.TrimSuffix(strings.ReplaceAll(path, "...}", "}"), "{$}")
		if route, _ := obs.CanonicalRoute(path); route == "unmatched-route" {
			unnamed = append(unnamed, pattern)
		}
	}
	slices.Sort(unnamed)
	if len(unnamed) > 0 {
		t.Errorf("%d routes have no telemetry name:\n%s", len(unnamed), strings.Join(unnamed, "\n"))
	}
}

// Over a real request: the label in the log is the route, not unmatched-route.
func TestANamedRouteIsLoggedByName(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	s := runServer(ts)
	dir := t.TempDir()
	sink, err := obs.OpenSink(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	s.Log = slog.New(obs.NewHandler(sink, slog.LevelInfo))
	for _, path := range []string{"/openapi.json", "/_localbase/runs/run_x", "/_localbase/usage"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	// A request's line is written after its response has gone, so it can still be on its way.
	wants := []string{`"path":"/openapi.json"`, `"path":"/_localbase/runs/{rest}"`, `"path":"/_localbase/usage"`}
	var b []byte
	WaitFor(10*time.Second, func() bool {
		b, _ = os.ReadFile(filepath.Join(dir, "events.jsonl"))
		for _, want := range wants {
			if !strings.Contains(string(b), want) {
				return false
			}
		}
		return true
	})
	for _, want := range wants {
		if !strings.Contains(string(b), want) {
			t.Errorf("no %s in the log", want)
		}
	}
	if strings.Contains(string(b), "unmatched-route") {
		t.Errorf("still unmatched: %s", b)
	}
}
