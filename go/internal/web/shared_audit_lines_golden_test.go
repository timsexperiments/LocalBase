package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
)

// The page's line for audit.model-backend-changed, which the command line writes too: a backend
// set, then reset, by a signed-in manager. The message and attributes as logged.
// LOCALBASE_UPDATE_AUDIT_GOLDEN=1 rewrites the golden.
func TestThePagesSharedAuditLines(t *testing.T) {
	p := newBackendPages(t)
	manager := p.ui("models:manage", "models:read")
	path := "/app/models/backend/qwen3.6-35b-a3b-iq4_nl_xl"
	for _, form := range []url.Values{{"backend": {"llama.cpp-cpu"}}, {"reset": {"1"}}} {
		if w := adminPost(manager, path, form); w.Code != http.StatusOK {
			t.Fatalf("%v: %d %.200s", form, w.Code, w.Body)
		}
	}
	p.logs.mu.Lock()
	text := p.logs.b.String()
	p.logs.mu.Unlock()
	var got strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil || m["event"] != backendChanged {
			continue
		}
		msg := m["msg"]
		for _, k := range []string{"time", "level", "msg"} {
			delete(m, k)
		}
		b, _ := json.Marshal(map[string]any{"message": msg, "attributes": m})
		got.Write(append(b, '\n'))
	}
	path = "testdata/shared_audit_lines.golden"
	if os.Getenv("LOCALBASE_UPDATE_AUDIT_GOLDEN") == "1" {
		if err := os.WriteFile(path, []byte(got.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != string(want) {
		t.Errorf("the lines changed:\n got:\n%s\nwant:\n%s", got.String(), want)
	}
}
