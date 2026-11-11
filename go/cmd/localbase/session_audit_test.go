package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// sessionEvent is one record of the root's audit log, with the attributes this file's events carry.
type sessionEvent struct {
	Event      string         `json:"eventName"`
	Attributes map[string]any `json:"attributes"`
}

func sessionEvents(t *testing.T, root string) []sessionEvent {
	t.Helper()
	f, err := os.Open(filepath.Join(root, "logs", "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []sessionEvent
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ev sessionEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

// The sign-in link and session commands each write their own record: access login-link issues one
// (its link id, never the token), access login-link --revoke ends every local session (a count),
// access sessions revoke <id> ends one (its id, and a count of 1), and access sessions revoke --all
// ends every session.
func TestTheSessionCommandsAreAudited(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	root := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	newSession := func() string {
		st, err := store.Open(filepath.Join(root, "local-base.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		_, id, err := auth.CreateLocalSession(context.Background(), st)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	last := func(want string) map[string]any {
		t.Helper()
		evs := sessionEvents(t, root)
		if len(evs) == 0 || evs[len(evs)-1].Event != want {
			t.Fatalf("the last record isn't %s: %+v", want, evs)
		}
		return evs[len(evs)-1].Attributes
	}
	run := func(args ...string) {
		t.Helper()
		if code, out := runApp(t, append([]string{"--root", root}, args...)...); code != 0 {
			t.Fatalf("%v: %d %s", args, code, out)
		}
	}

	run("access", "login-link")
	if a := last("audit.local-login-issued"); a["actor_kind"] != "cli" || a["forced"] != false || a["link_id"] == "" || a["link_id"] == nil {
		t.Errorf("login-link: %v", a)
	}

	one := newSession()
	newSession()
	run("access", "sessions", "revoke", one)
	if a := last("audit.local-sessions-revoked"); a["session_id"] != one || a["count"] != float64(1) || a["actor_kind"] != "cli" {
		t.Errorf("sessions revoke <id>: %v", a)
	}

	run("access", "login-link", "--revoke")
	if a := last("audit.local-sessions-revoked"); a["session_id"] != nil || a["count"] != float64(1) || a["actor_kind"] != "cli" {
		t.Errorf("login-link --revoke: %v (one session was left)", a)
	}

	newSession()
	newSession()
	run("access", "sessions", "revoke", "--all")
	if a := last("audit.sessions-revoked"); a["count"] != float64(2) || a["actor_kind"] != "cli" {
		t.Errorf("sessions revoke --all: %v", a)
	}
}
