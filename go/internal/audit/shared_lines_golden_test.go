package audit

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/root"
)

// The command line's lines for the two events the gateway writes too (audit.local-sessions-revoked
// and audit.model-backend-changed), as the sink writes them, with each line's id and time left out.
// The gateway's are pinned beside its own writers (internal/gateway, internal/web).
// LOCALBASE_UPDATE_AUDIT_GOLDEN=1 rewrites the golden from the code as it is.
func TestTheCommandLinesSharedAuditLines(t *testing.T) {
	r, err := root.Init(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	l := New(r, io.Discard)
	l.LocalSessionsRevoked(3)
	l.LocalSessionRevoked("sess_1")
	l.BackendChanged("set", "m.1", "b.1")
	l.BackendChanged("reset", "m.1", "")
	l.BackendChanged("import", "m.1", "b.1")
	raw, err := os.ReadFile(filepath.Join(r.LogsDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		delete(m, "id")
		delete(m, "timestamp")
		b, _ := json.Marshal(m)
		out.Write(append(b, '\n'))
	}
	compareAuditGolden(t, "testdata/shared_audit_lines.golden", out.String())
}

func compareAuditGolden(t *testing.T, path, got string) {
	t.Helper()
	if os.Getenv("LOCALBASE_UPDATE_AUDIT_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("the lines changed:\n got:\n%s\nwant:\n%s", got, want)
	}
}
