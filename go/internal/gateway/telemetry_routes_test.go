package gateway

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/obs"
)

// Every route the gateway serves has a name in the telemetry, or its requests are all counted as
// "unmatched-route" and nobody can tell them apart. Reads the source, so a new route is covered
// without being named here.
func TestEveryRouteHasATelemetryName(t *testing.T) {
	b, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	routes := regexp.MustCompile(`mux\.Handle(?:Func)?\("(?:[A-Z]+ )?([^"]+)"`).FindAllStringSubmatch(string(b), -1)
	if len(routes) < 30 {
		t.Fatalf("found only %d routes: the pattern no longer reads server.go", len(routes))
	}
	var unnamed []string
	for _, r := range routes {
		// As routeLabel makes it from the request's pattern.
		path := strings.TrimSuffix(strings.ReplaceAll(r[1], "...}", "}"), "{$}")
		if route, _ := obs.CanonicalRoute(path); route == "unmatched-route" {
			unnamed = append(unnamed, r[1])
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
	b, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"path":"/openapi.json"`, `"path":"/_localbase/runs/{rest}"`, `"path":"/_localbase/usage"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("no %s in the log", want)
		}
	}
	if strings.Contains(string(b), "unmatched-route") {
		t.Errorf("still unmatched: %s", b)
	}
}
