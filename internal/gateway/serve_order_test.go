package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
)

// requestLog is a gateway whose log a test reads, one JSON record per line.
func requestLog(t *testing.T) (*gatewaytest.SyncBuffer, func(method, path string) int) {
	t.Helper()
	logs := &gatewaytest.SyncBuffer{}
	ts := newTestServerConfigured(t, func(c *Config) { c.Log = slog.New(slog.NewJSONHandler(logs, nil)) })
	runServer(ts).mux.HandleFunc("GET /test/panic-for-log", func(http.ResponseWriter, *http.Request) { panic(errors.New("boom")) })
	do := func(method, path string) int {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	return logs, do
}

// logRecord is one line of the log: its event and, for http.request, what it says of the request.
type logRecord struct {
	Event string `json:"event"`
	HTTP  struct {
		Method string `json:"method"`
		Path   string `json:"path"`
		Status int    `json:"status"`
	} `json:"http"`
}

func logRecords(t *testing.T, logs *gatewaytest.SyncBuffer) []logRecord {
	t.Helper()
	var out []logRecord
	for _, line := range strings.Split(logs.String(), "\n") {
		var r logRecord
		if line != "" && json.Unmarshal([]byte(line), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

// A panic is logged as http.panic, and then the request is logged as answered 503: the recover
// comes before the request log, in the one deferred finish.
func TestAPanicIsLoggedBeforeTheRequestItAnswered(t *testing.T) {
	logs, do := requestLog(t)
	if status := do("GET", "/test/panic-for-log"); status != 503 {
		t.Fatalf("the panic answered %d", status)
	}
	panicAt, requestAt := -1, -1
	for i, r := range logRecords(t, logs) {
		switch {
		case r.Event == "http.panic" && panicAt < 0:
			panicAt = i
		case r.Event == "http.request" && r.HTTP.Status == 503 && requestAt < 0:
			requestAt = i
		}
	}
	if panicAt < 0 || requestAt < 0 || panicAt > requestAt {
		t.Errorf("want http.panic before the http.request 503: panic at %d, request at %d", panicAt, requestAt)
	}
}

// An OPTIONS refused before any route runs is logged under the route it was for, as the route
// table names it, not as an unmatched path.
func TestARefusedOptionsIsLoggedUnderItsRoute(t *testing.T) {
	logs, do := requestLog(t)
	if status := do("OPTIONS", "/v1/models"); status != 403 {
		t.Fatalf("OPTIONS answered %d", status)
	}
	for _, r := range logRecords(t, logs) {
		if r.Event == "http.request" && r.HTTP.Method == "OPTIONS" {
			if r.HTTP.Path != "/v1/models" || r.HTTP.Status != 403 {
				t.Errorf("the refused OPTIONS was logged as %s %d, want /v1/models 403", r.HTTP.Path, r.HTTP.Status)
			}
			return
		}
	}
	t.Error("the refused OPTIONS wasn't logged")
}

// A successful probe isn't request-logged (quietRoute), and the request beside it is.
func TestASuccessfulProbeIsNotLogged(t *testing.T) {
	logs, do := requestLog(t)
	if status := do("GET", "/health"); status != 200 {
		t.Fatalf("/health answered %d", status)
	}
	do("GET", "/v1/models")
	var health, models bool
	for _, r := range logRecords(t, logs) {
		if r.Event == "http.request" {
			health = health || r.HTTP.Path == "/health"
			models = models || r.HTTP.Path == "/v1/models"
		}
	}
	if !models {
		t.Fatal("the request beside the probe wasn't logged, so this can't tell")
	}
	if health {
		t.Error("a successful /health was request-logged")
	}
}
