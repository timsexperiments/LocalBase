package main

import (
	"bufio"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// keyCreatedLines is every api-key create record in the root's audit log, in order.
func keyCreatedLines(t *testing.T, root string) []map[string]any {
	t.Helper()
	f, err := os.Open(filepath.Join(root, "logs", "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ev struct {
			EventName  string         `json:"eventName"`
			Message    string         `json:"message"`
			Attributes map[string]any `json:"attributes"`
		}
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.EventName == "audit.api-key-changed" && ev.Attributes["action"] == "create" {
			ev.Attributes["message"] = ev.Message
			out = append(out, ev.Attributes)
		}
	}
	return out
}

// connect --create-key logs the key it creates in the line keys create writes for the same key:
// the same message and attributes, its actor_id and its scopes among them. Only the key's id
// differs. (Before, connect wrote a line of its own, with no actor_id and no scopes.)
func TestConnectLogsTheKeyItCreatesAsKeysCreateDoes(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	root := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	if code, out := runApp(t, "--root", root, "--json", "connect", "curl", "--create-key"); code != 0 {
		t.Fatalf("connect: %s", out)
	}
	if code, out := runApp(t, "--root", root, "--json", "keys", "create", "--name", "k", "--scopes", strings.Join(connectScopes, ",")); code != 0 {
		t.Fatalf("keys create: %s", out)
	}
	lines := keyCreatedLines(t, root)
	if len(lines) != 2 {
		t.Fatalf("want two key-create records, got %d: %v", len(lines), lines)
	}
	connect, keys := lines[0], lines[1]
	if connect["scopes"] != "inference:chat,models:read" || connect["actor_id"] != "" || connect["target_id"] == keys["target_id"] {
		t.Errorf("connect's line: %v", connect)
	}
	delete(connect, "target_id")
	delete(keys, "target_id")
	if !maps.Equal(connect, keys) {
		t.Errorf("connect's line isn't keys create's:\nconnect %v\nkeys    %v", connect, keys)
	}
}
