package gateway

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
)

// The instance token is for the local command line, and gets it one thing here: cancelling.
func TestInstanceTokenCanOnlyCancelAnInstall(t *testing.T) {
	ts := openAPIGateway(t)
	s := runServer(ts)
	release, err := s.Root.Lock("127.0.0.1", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	token := s.Root.Instance().InstanceToken
	send := func(method, body, tok string) int {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+"/_localbase/model-management", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if tok != "" {
			req.Header.Set("X-Localbase-Instance-Token", tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	const model = "qwen3.5-4b-q4_k_m"
	if got := send("POST", `{"modelId":"`+model+`","action":"cancel"}`, token); got != 409 {
		t.Errorf("cancel with the token, nothing running: %d", got)
	}
	for _, action := range []string{"install", "uninstall", "enable", "disable", "activate"} {
		if got := send("POST", `{"modelId":"`+model+`","action":"`+action+`"}`, token); got != 403 {
			t.Errorf("%s with the token: %d", action, got)
		}
	}
	if got := send("GET", "", token); got != 401 {
		t.Errorf("reading with the token: %d", got)
	}
	if got := send("POST", `{"modelId":"`+model+`","action":"cancel"}`, "wrong"); got != 401 {
		t.Errorf("a wrong token: %d", got)
	}
	if got := send("POST", `{"modelId":"`+model+`","action":"cancel"}`, ""); got != 401 {
		t.Errorf("no token: %d", got)
	}
}

// The audit trail names who cancelled, for the command line as for a key.
func TestACancelFromTheCommandLineIsAudited(t *testing.T) {
	ts := openAPIGateway(t)
	s := runServer(ts)
	release, err := s.Root.Lock("127.0.0.1", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var logs bytes.Buffer
	s.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	const model = "qwen3.5-4b-q4_k_m"
	// A finished operation answers a cancel with how it ended.
	s.mgmt.mu.Lock()
	s.mgmt.operations[model] = &mgmtOperation{Action: "install", State: "cancelled", Detail: cancelDetail}
	s.mgmt.mu.Unlock()
	req, _ := http.NewRequest("POST", ts.URL+"/_localbase/model-management", strings.NewReader(`{"modelId":"`+model+`","action":"cancel"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Localbase-Instance-Token", s.Root.Instance().InstanceToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("%d", resp.StatusCode)
	}
	found := false
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `"audit.models-changed"`) {
			found = true
			for _, want := range []string{`"actor_kind":"cli"`, `"action":"cancel"`, `"target_id":"` + model + `"`} {
				if !strings.Contains(line, want) {
					t.Errorf("%s lacks %s", line, want)
				}
			}
		}
	}
	if !found {
		t.Errorf("no audit event: %s", logs.String())
	}
}

// The document says so, as the permission it names isn't the whole of it.
func TestTheDocumentDescribesTheTokenException(t *testing.T) {
	b, err := os.ReadFile("openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]struct {
			Post struct {
				Description string `json:"description"`
				Permission  string `json:"x-localbase-permission"`
			} `json:"post"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	op := doc.Paths["/_localbase/model-management"].Post
	if op.Permission != "models:manage" || !strings.Contains(op.Description, "or the instance token, cancel only") {
		t.Errorf("%+v", op)
	}
}
