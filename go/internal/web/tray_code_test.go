package web

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

// A run's error code can be a number: llama-server's streamed errors carry code 400, and the run
// list and a run's read keep it (#841). One such run must not empty the Studio tray, and a failed
// run with a numeric code still says what went wrong, in the tray and in its own refresh.
func TestTheTrayReadsARunWhoseErrorCodeIsANumber(t *testing.T) {
	image := `{"id":"run_1","target":"/v1/images/generations","status":"failed","label":"Image","error":{"type":"server_error","code":500,"message":"The picture model stopped."}}`
	chat := `{"id":"run_c","target":"/v1/chat/completions","status":"failed","error":{"type":"exceed_context_size_error","code":400,"message":"the request exceeds the available context size"}}`
	call := func(_ context.Context, _ *auth.Principal, method, path, _ string, _ []byte) APIResult {
		switch {
		case method == "GET" && path == "/_localbase/runs":
			return APIResult{Status: 200, Body: []byte(`{"object":"list","data":[` + image + `,` + chat + `]}`)}
		case method == "GET" && path == "/_localbase/runs/run_1":
			return APIResult{Status: 200, Body: []byte(image)}
		}
		return APIResult{Status: 404, Body: []byte(`{"error":{"message":"not found"}}`)}
	}
	u := New(Deps{NoAuth: true, Call: call})
	page := get(u, "/app/studio/image").Body.String()
	if !strings.Contains(page, `<li id="run-run_1"`) || !strings.Contains(page, "The picture model stopped.") {
		t.Errorf("the tray lost its runs to a numeric code: %.300s", page)
	}
	if one := get(u, "/app/lab/runs/run_1").Body.String(); !strings.Contains(one, "The picture model stopped.") {
		t.Errorf("the run's own refresh: %.300s", one)
	}
}

// The code is kept as a text: a number as its digits, anything else as no code.
func TestARunErrorCodeIsReadAsText(t *testing.T) {
	for raw, want := range map[string]string{`"model_unloading"`: "model_unloading", `400`: "400", `true`: "", `{"a":1}`: "", `null`: ""} {
		var run labRun
		if err := json.Unmarshal([]byte(`{"id":"r","error":{"code":`+raw+`,"message":"m"}}`), &run); err != nil || run.Error == nil || string(run.Error.Code) != want || run.Error.Message != "m" {
			t.Errorf("code %s: %v %+v", raw, err, run.Error)
		}
	}
}
