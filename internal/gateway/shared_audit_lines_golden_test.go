package gateway

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
	"github.com/timsexperiments/LocalBase/go/internal/placement/placementtest"
)

// The gateway's lines for the two events the command line writes too (audit.local-sessions-revoked
// and audit.model-backend-changed), from each writer: the local-sessions route (all, and one), the
// model-backend route, and an import that chooses a backend. Each is the message and attributes as
// logged; the API key's id is masked. LOCALBASE_UPDATE_AUDIT_GOLDEN=1 rewrites the golden.
func TestTheGatewaysSharedAuditLines(t *testing.T) {
	f := newAuditFixture(t)
	f.srv.changeAudit.sampler.Burst = 1000
	source := filepath.Join(scratchDir(t), "golden-weights-q4_k_m.gguf")
	placementtest.WriteMoEGGUF(t, source, 1<<20)
	from := len(f.logs.String())
	for _, req := range []struct{ path, body string }{
		{"/_localbase/local-sessions", `{"action":"revoke-all"}`},
		{"/_localbase/local-sessions", `{"action":"revoke","id":"sess_1"}`},
		{"/_localbase/model-backend", `{"action":"reset","model":"qwen3.5-4b-q4_k_m"}`},
		{"/_localbase/model-imports", `{"action":"start","source":` + quote(source) + `,"task":"chat","id":"golden-import","name":"Golden","backend":"llama.cpp-cpu"}`},
	} {
		status, out := f.send(f.key, "POST", req.path, "application/json", []byte(req.body))
		if status >= 300 {
			t.Fatalf("%s %s: %d %s", req.path, req.body, status, out)
		}
		if req.path == "/_localbase/model-imports" {
			// The import runs on: wait for it to end, so nothing writes into the root after the test.
			var started struct{ Job string }
			_ = json.Unmarshal(out, &started)
			if !gatewaytest.WaitFor(openAPIWait, func() bool {
				_, b := f.send(f.key, "GET", "/_localbase/model-imports?id="+started.Job, "", nil)
				return !bytes.Contains(b, []byte(`"state":"running"`))
			}) {
				t.Fatal("the import did not finish")
			}
		}
	}
	var got strings.Builder
	for _, line := range strings.Split(f.logs.String()[from:], "\n") {
		if !strings.Contains(line, `"event":"audit.local-sessions-revoked"`) && !strings.Contains(line, `"event":"audit.model-backend-changed"`) {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		msg := m["msg"]
		for _, k := range []string{"time", "level", "msg"} {
			delete(m, k)
		}
		if m["actor_id"] == f.id {
			m["actor_id"] = "<the API key's id>"
		}
		b, _ := json.Marshal(map[string]any{"message": msg, "attributes": m})
		got.Write(append(b, '\n'))
	}
	path := "testdata/shared_audit_lines.golden"
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

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }
