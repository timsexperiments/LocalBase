package audit

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/root"
)

// lineOf writes one record through a fresh Log and returns every line it wrote, each with the log
// file it went to (events.jsonl always; audit.jsonl too for an audit.* event), without the two
// fields that differ every time (id and timestamp). Which files are part of the record.
func lineOf(t *testing.T, write func(*Log)) string {
	t.Helper()
	r, err := root.Init(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	write(New(r, nil))
	files, err := filepath.Glob(filepath.Join(r.LogsDir(), "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if line != "" {
				got = append(got, filepath.Base(f)+" "+varying.ReplaceAllString(line, ""))
			}
		}
	}
	if len(got) == 0 {
		t.Fatal("nothing was written")
	}
	return strings.Join(got, "\n")
}

var varying = regexp.MustCompile(`"id":"[^"]*",|,"timestamp":"[^"]*"`)

// Each constructor writes exactly the line its call site wrote by hand before it existed. The
// "was" side is that call, copied as it was in cmd/localbase (5-2b-M, e16af79a): message, event
// name, attribute names and values, and the files it goes to.
func TestEachEventWritesTheLineItsCallWrote(t *testing.T) {
	for _, tc := range []struct {
		name     string
		was, now func(*Log)
	}{
		{"LocalLinkIssued",
			func(l *Log) {
				l.Record("Local sign-in link issued.", "event", "audit.local-login-issued", "link_id", "a1b2c3d4",
					"actor_kind", "cli", "forced", true)
			},
			func(l *Log) { l.LocalLinkIssued("a1b2c3d4", true) }},
		{"LocalSessionsRevoked",
			func(l *Log) {
				l.Record("Local sessions revoked.", "event", "audit.local-sessions-revoked", "actor_kind", "cli", "actor_id", "", "count", 3)
			},
			func(l *Log) { l.LocalSessionsRevoked(3) }},
		{"LocalSessionRevoked",
			func(l *Log) {
				l.Record("Local sessions revoked.", "event", "audit.local-sessions-revoked", "actor_kind", "cli", "actor_id", "", "session_id", "sess_1", "count", 1)
			},
			func(l *Log) { l.LocalSessionRevoked("sess_1") }},
		{"SessionsRevoked",
			func(l *Log) {
				l.Record("All sessions revoked.", "event", "audit.sessions-revoked", "actor_kind", "cli", "count", 5)
			},
			func(l *Log) { l.SessionsRevoked(5) }},
		{"BackendChanged",
			func(l *Log) {
				l.Record("Administration change.", "event", "audit.model-backend-changed", "action", "set", "actor_kind", "cli", "actor_id", "", "target_kind", "model", "target_id", "m.1", "backend_id", "b.1")
			},
			func(l *Log) { l.BackendChanged("set", "m.1", "b.1") }},
		{"BackendChanged reset",
			func(l *Log) {
				l.Record("Administration change.", "event", "audit.model-backend-changed", "action", "reset", "actor_kind", "cli", "actor_id", "", "target_kind", "model", "target_id", "m.1", "backend_id", "")
			},
			func(l *Log) { l.BackendChanged("reset", "m.1", "") }},
		{"BackendRegistered",
			func(l *Log) {
				l.Record("Administration change.", "event", "audit.backend-registered", "action", "add", "actor_kind", "cli", "target_kind", "backend", "target_id", "b.1", "backend_id", "b.1")
			},
			func(l *Log) { l.BackendRegistered("b.1") }},
		{"BackendRemoved",
			func(l *Log) {
				l.Record("Administration change.", "event", "audit.backend-removed", "action", "remove", "actor_kind", "cli", "target_kind", "backend", "target_id", "b.1", "backend_id", "b.1")
			},
			func(l *Log) { l.BackendRemoved("b.1") }},
		{"BackupCreated",
			func(l *Log) {
				l.Record("backup.created", "event", "backup.created", "category", "audit", "component", "backup",
					"secretsIncluded", true, "bytes", int64(12345))
			},
			func(l *Log) { l.BackupCreated(true, 12345) }},
		{"BackupRestored",
			func(l *Log) {
				l.Record("backup.restored", "event", "backup.restored", "category", "audit", "component", "backup",
					"secretsIncluded", false, "revocationsKept", 2,
					"keysKept", 3, "keysLeftOut", 1, "keysHeldBack", 4, "keysBroughtBack", "key_a,key_b")
			},
			func(l *Log) {
				l.BackupRestored(Restored{SecretsIncluded: false, RevocationsKept: 2, KeysKept: 3, KeysLeftOut: 1, KeysHeldBack: 4,
					KeysBroughtBack: []string{"key_a", "key_b"}})
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if was, now := lineOf(t, tc.was), lineOf(t, tc.now); was != now {
				t.Errorf("the line changed:\nwas %s\nnow %s", was, now)
			}
		})
	}
}
