package system

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/testutil"
	"github.com/timsexperiments/LocalBase/go/test/harness"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// A real gateway and a real flood: the main log barely grows, the audit event stays, and the
// command line can read it.
func TestAuditEventsSurviveAFlood(t *testing.T) {
	if harness.Impl() != "go" && harness.Impl() != "both" {
		t.Skip("runs the Go gateway")
	}
	tmp := fsguard.ScratchDir(t)
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
	if err := testutil.Flood(1, flood, func(c *http.Client) {
		resp, err := c.Get(e.base + "/v1/models")
		if err != nil {
			t.Error(err)
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}); err != nil {
		t.Fatal(err)
	}
	const refusedChanges = 3
	for range refusedChanges {
		resp, err := http.Post(e.base+"/_localbase/api-keys", "application/json", strings.NewReader(`{"action":"create","name":"x","scopes":["keys:read"]}`))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("a change with no credential: %d", resp.StatusCode)
		}
	}
	// Without the limit this is about 9 MB.
	if grew := size("events.jsonl") - before; grew > 64<<10 {
		t.Errorf("the main log grew %d bytes for %d requests with no credential", grew, flood)
	}
	e.stop()
	// Nothing came after the flood to bring its count out, so stopping wrote it.
	main, err := os.ReadFile(filepath.Join(logDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var last map[string]any
	for _, line := range strings.Split(string(main), "\n") {
		var ev struct {
			EventName  string         `json:"eventName"`
			Attributes map[string]any `json:"attributes"`
		}
		if json.Unmarshal([]byte(line), &ev) == nil && ev.EventName == "http.anonymous-summary" {
			last = ev.Attributes
		}
	}
	if last == nil || last["refused_total"] != float64(flood+refusedChanges) || last["admin_refused_total"] != float64(refusedChanges) {
		t.Errorf("the summary at shutdown: %v", last)
	}
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

// Any key can be refused, and refusals share a file with the record of who changed what. A real
// gateway, and a key with no admin rights refused for longer than that file keeps: the record stays.
func TestRefusedChangesCannotPushOutWhoChangedWhat(t *testing.T) {
	if harness.Impl() != "go" && harness.Impl() != "both" {
		t.Skip("runs the Go gateway")
	}
	tmp := fsguard.ScratchDir(t)
	e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp}
	if r := e.cli(nil, "--json", "init"); r.code != 0 {
		t.Fatalf("init: %s %s", r.stdout, r.stderr)
	}
	admin, _ := e.json("keys", "create", "--name", "admin", "--scopes", "keys:manage,keys:read")["secret"].(string)
	reader, _ := e.json("keys", "create", "--name", "reader", "--scopes", "models:read")["secret"].(string)
	if admin == "" || reader == "" {
		t.Fatal("no keys")
	}
	e.serve()
	defer e.stop()
	post := func(c *http.Client, key string, body io.Reader) int {
		t.Helper()
		req, _ := http.NewRequest("POST", e.base+"/_localbase/api-keys", body)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := c.Do(req)
		if err != nil {
			t.Error(err)
			return 0
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	change := func() {
		t.Helper()
		if code := post(http.DefaultClient, admin, strings.NewReader(`{"action":"create","name":"x","scopes":["keys:read"]}`)); code != 201 {
			t.Fatalf("the change: %d", code)
		}
	}
	change()
	logs := filepath.Join(e.root, "logs")
	before, err := os.Stat(filepath.Join(logs, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// Before this limit, about 160,000 of these took the record out of every file. They carry no
	// body, because a refusal closes a connection whose body it hasn't read.
	const flood = 170_000
	var wrong atomic.Int64
	if err := testutil.Flood(8, flood, func(c *http.Client) {
		if post(c, reader, nil) != 403 {
			wrong.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if n := wrong.Load(); n != 0 {
		t.Errorf("%d answers weren't a refusal", n)
	}
	after, err := os.Stat(filepath.Join(logs, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if grew := after.Size() - before.Size(); grew > 64<<10 {
		t.Errorf("the audit file grew %d bytes for %d refusals", grew, flood)
	}
	change() // while refusals are being held back
	e.stop()
	var kept strings.Builder
	matches, _ := filepath.Glob(filepath.Join(logs, "audit*.jsonl"))
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		kept.Write(b)
	}
	// Two made here, and the two keys the command line created for this test.
	if n := strings.Count(kept.String(), "audit.api-key-changed"); n != 4 {
		t.Errorf("%d records of the 4 changes are left, in %d files", n, len(matches))
	}
	// What was held back is said when the gateway stops, in the file it was held back from.
	if !strings.Contains(kept.String(), "audit.refused-summary") {
		t.Error("no summary of the refusals")
	}
}
