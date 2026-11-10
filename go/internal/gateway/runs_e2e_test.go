package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
)

// The runs tests run against a stub (runs/harness_test.go). These, with runs_test.go's tests that
// go through a whole gateway (create and stream, resume, cancel, results, restart), keep the real
// relay and the fake backend in the loop, through HTTP only, so the stub can't drift from what the
// real routes send unnoticed.

// A run whose body can't fit in what runs may hold together is refused in the documented shape: a
// run that has already failed, its reason, and the error a page shows.
func TestARunRefusedForMemoryEndToEnd(t *testing.T) {
	ts := openAPIGateway(t, func(c *Config) { c.RunsMemoryLimit = 1 }) // no body fits
	s := runServer(ts)
	_, key, err := s.cfg.Store.CreateAPIKey(t.Context(), "all", auth.Permissions)
	if err != nil {
		t.Fatal(err)
	}
	d := loadOpenAPI(t)
	req, err := http.NewRequest("POST", ts.URL+"/_localbase/runs/v1/chat/completions", bytes.NewReader([]byte(gatewaytest.ChatBody("hi"))))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	d.checkResponse(t, "POST", "/_localbase/runs/v1/chat/completions", resp.StatusCode, resp.Header, out)
	var run struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
		Error  struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if resp.StatusCode != 202 || json.Unmarshal(out, &run) != nil {
		t.Fatalf("%d %s", resp.StatusCode, firstBytes(out))
	}
	if run.Status != "failed" || run.Reason != "memory_limit" || run.Error.Code != "runs_memory_full" {
		t.Errorf("a run over the total: status %q, reason %q, code %q", run.Status, run.Reason, run.Error.Code)
	}
}
