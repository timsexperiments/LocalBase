package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

// A run that outlasts its maximum duration is interrupted, and says so in the documented shape: its
// status, why, and how a client continues. The run is one known to outlast the limit by far, so
// what is checked is the shape, not the timing.
func TestARunPastItsMaxDurationIsInterrupted(t *testing.T) {
	ts := openAPIGateway(t, func(s *Server) { s.RunMaxDuration = 200 * time.Millisecond })
	s := runServer(ts)
	_, key, err := s.Store.CreateAPIKey(t.Context(), "all", auth.Permissions)
	if err != nil {
		t.Fatal(err)
	}
	d := loadOpenAPI(t)
	do := func(method, path, body string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, ts.URL+path, bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		d.checkResponse(t, method, path, resp.StatusCode, resp.Header, out)
		return resp.StatusCode, out
	}

	// fake:slow 200 streams 200 words 50 ms apart: about ten seconds, against a limit of 200 ms.
	status, out := do("POST", "/_localbase/runs/v1/chat/completions", chatBody("fake:slow 200"))
	var created struct {
		ID string `json:"id"`
	}
	if status != 202 || json.Unmarshal(out, &created) != nil || created.ID == "" {
		t.Fatalf("creating the run: %d %s", status, firstBytes(out))
	}
	var run struct {
		Status       string          `json:"status"`
		Reason       string          `json:"reason"`
		EndedAt      *string         `json:"endedAt"`
		Continuation json.RawMessage `json:"continuation"`
	}
	if !waitFor(openAPIWait, func() bool {
		_, out = do("GET", "/_localbase/runs/"+created.ID, "")
		return json.Unmarshal(out, &run) == nil && run.EndedAt != nil
	}) {
		t.Fatalf("the run did not end: %s", firstBytes(out))
	}
	if run.Status != "interrupted" || run.Reason != "max_duration" || len(run.Continuation) == 0 {
		t.Errorf("an interrupted run: status %q, reason %q, continuation %s", run.Status, run.Reason, run.Continuation)
	}
}
