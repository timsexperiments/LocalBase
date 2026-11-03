package contract

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/testutil"
)

// A key that can only manage models hammers a route it may use. Its changes are limited and
// counted, and the record of who changed keys stays in the file, before, during and after. Then
// every log is searched for what the requests carried.
func TestLimitedChangesCannotPushOutWhoChangedKeys(t *testing.T) {
	if *implFlag != "go" && *implFlag != "both" {
		t.Skip("runs the Go gateway")
	}
	tmp := scratchDir(t)
	e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp}
	if r := e.cli(nil, "--json", "init"); r.code != 0 {
		t.Fatalf("init: %s %s", r.stdout, r.stderr)
	}
	admin, _ := e.json("keys", "create", "--name", "admin", "--scopes", "keys:manage,keys:read")["secret"].(string)
	manager, _ := e.json("keys", "create", "--name", "manager", "--scopes", "models:manage,models:read")["secret"].(string)
	if admin == "" || manager == "" {
		t.Fatal("no keys")
	}
	e.serve()
	defer e.stop()
	send := func(c *http.Client, method, path, key, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, e.base+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			t.Error(err)
			return 0, ""
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}
	secrets := map[string]string{"the admin key": admin, "the manager's key": manager}
	const created = "only-a-change-to-count-"
	var keyChanges int
	change := func() {
		t.Helper()
		status, out := send(http.DefaultClient, "POST", "/_localbase/api-keys", admin, `{"action":"create","name":"x","scopes":["keys:read"]}`)
		if status != 200 && status != 201 {
			t.Fatalf("the change: %d %s", status, out)
		}
		_, after, _ := strings.Cut(out, `"secret":"`)
		secret, _, _ := strings.Cut(after, `"`)
		secrets["a key made here"] = secret
		keyChanges++
	}
	const token = "hf_PLANTEDfloodTokenValue0123456789abcd"
	secrets["a Hugging Face token"] = token
	if status, out := send(http.DefaultClient, "POST", "/_localbase/hf-token", manager, `{"action":"save","token":"`+token+`","validate":false}`); status != 200 {
		t.Fatalf("token: %d %s", status, out)
	}
	if status, out := send(http.DefaultClient, "POST", "/_localbase/model-imports", manager, `{"action":"start","source":"https://user:PLANTED_url_password@example.invalid/m.gguf","task":"chat","authProfile":"PLANTED_profile_header_value"}`); status < 400 {
		t.Fatalf("a source that can't be used: %d %s", status, out)
	}
	secrets["a password in a source URL"] = "PLANTED_url_password"
	secrets["an auth profile value"] = "PLANTED_profile_header_value"
	change()
	logs := filepath.Join(e.root, "logs")
	before, err := os.Stat(filepath.Join(logs, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	const flood = 20_000
	var wrong atomic.Int64
	started := time.Now()
	if err := testutil.Flood(8, flood, func(c *http.Client) {
		if status, _ := send(c, "PUT", "/_localbase/computer-name", manager, `{"name":"`+created+`"}`); status != 200 {
			wrong.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	took := time.Since(started)
	if n := wrong.Load(); n != 0 {
		t.Errorf("%d answers weren't a change", n)
	}
	after, err := os.Stat(filepath.Join(logs, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	grew := after.Size() - before.Size()
	t.Logf("%d changes in %v, %.0f a second; the audit file grew %d bytes, %.0f bytes a second", flood, took.Round(time.Millisecond), float64(flood)/took.Seconds(), grew, float64(grew)/took.Seconds())
	if grew > 64<<10 {
		t.Errorf("the audit file grew %d bytes for %d changes", grew, flood)
	}
	change() // while the limit is in force
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
	// Those made here, and the two the command line created for this test.
	if n := strings.Count(kept.String(), "audit.api-key-changed"); n != keyChanges+2 {
		t.Errorf("%d records of the %d key changes", n, keyChanges+2)
	}
	if !strings.Contains(kept.String(), "audit.changes-summary") || !strings.Contains(kept.String(), "computer-name=") {
		t.Error("no summary saying what was counted, and where")
	}
	if strings.Contains(kept.String(), created) {
		t.Error("the computer name is in the audit file")
	}
	everything, _ := filepath.Glob(filepath.Join(logs, "*"))
	if len(everything) == 0 {
		t.Fatal("no logs")
	}
	for _, m := range everything {
		b, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		for what, value := range secrets {
			if value != "" && strings.Contains(string(b), value) {
				t.Errorf("%s holds %s", m, what)
			}
		}
	}
}
