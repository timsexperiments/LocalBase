package contract

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A real gateway and a real flood: the main log barely grows, the audit event stays, and the
// command line can read it.
func TestAuditEventsSurviveAFlood(t *testing.T) {
	if *implFlag != "go" && *implFlag != "both" {
		t.Skip("runs the Go gateway")
	}
	tmp := scratchDir(t)
	e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp}
	if r := e.cli(nil, "--json", "init"); r.code != 0 {
		t.Fatalf("init: %s %s", r.stdout, r.stderr)
	}
	created := e.json("keys", "create", "--name", "admin", "--scopes", "keys:manage,keys:read")
	secret, _ := created["secret"].(string)
	if secret == "" {
		t.Fatalf("no secret: %v", created)
	}
	e.serve()
	defer e.stop()
	req, _ := http.NewRequest("POST", e.base+"/_localbase/api-keys", strings.NewReader(`{"action":"create","name":"planted-audit-name","scopes":["keys:read"]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	logDir := filepath.Join(e.root, "logs")
	size := func(name string) int64 {
		info, err := os.Stat(filepath.Join(logDir, name))
		if err != nil {
			return 0
		}
		return info.Size()
	}
	before := size("events.jsonl")
	const flood = 20000
	for range flood {
		resp, err := http.Get(e.base + "/v1/models")
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	// Without the limit this is about 9 MB.
	if grew := size("events.jsonl") - before; grew > 64<<10 {
		t.Errorf("the main log grew %d bytes for %d requests with no credential", grew, flood)
	}
	e.stop()
	b, err := os.ReadFile(filepath.Join(logDir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "audit.api-key-changed") || strings.Contains(string(b), "planted-audit-name") {
		t.Errorf("audit.jsonl: %.300s", b)
	}
	r := e.cli(nil, "--json", "logs", "--audit")
	if r.code != 0 {
		t.Fatalf("logs --audit: %s %s", r.stdout, r.stderr)
	}
	var out struct {
		Data struct {
			Events []map[string]any `json:"events"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
		t.Fatalf("%v: %s", err, r.stdout)
	}
	found := false
	for _, ev := range out.Data.Events {
		name, _ := ev["eventName"].(string)
		if !strings.HasPrefix(name, "audit.") {
			t.Errorf("logs --audit gave %s", name)
		}
		found = found || name == "audit.api-key-changed"
	}
	if !found {
		t.Errorf("logs --audit: %.400s", r.stdout)
	}
	if r := e.cli(nil, "logs", "--audit", "--follow"); r.code == 0 {
		t.Errorf("logs --audit --follow was accepted: %s", r.stdout)
	}
}
