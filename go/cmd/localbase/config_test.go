package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The TS apply cases (apply.test.ts) as decisions.
func TestApplyDecision(t *testing.T) {
	for _, c := range []struct {
		policy         string
		wait, required bool
		state          string
		manager        bool
		restart        bool
		err            string
	}{
		{"auto", false, true, "stopped", true, true, ""},       // auto starts a stopped managed service
		{"auto", false, true, "not_installed", true, true, ""}, // and an uninstalled one
		{"auto", false, true, "failed", true, true, ""},        // a failed restart is retried
		{"always", true, false, "running", true, true, ""},     // always restarts without changes
		{"auto", false, false, "running", true, false, ""},     // hot changes never restart
		{"never", false, true, "running", true, false, ""},     // deferred
		{"auto", false, true, "foreground", true, false, "Cannot automatically restart"},
		{"auto", false, true, "unknown", true, false, "Cannot automatically restart"},
		{"auto", false, true, "stopping", true, false, "Cannot automatically restart"},
		{"always", false, false, "foreground", true, false, "Cannot safely restart"},
		{"always", false, false, "stopped", false, false, "Cannot safely restart"},
		{"auto", false, true, "stopped", false, false, "Cannot safely restart"},
		{"never", true, true, "running", true, false, "--wait requires"}, // wait rejects deferred restart changes
		{"auto", true, false, "stopped", true, false, "--wait requires"},
		{"auto", true, false, "not_installed", true, false, "--wait requires"},
		{"auto", true, false, "foreground", true, false, ""}, // waits for a foreground gateway
		{"never", true, false, "running", true, false, ""},   // and a running one
		{"auto", true, true, "running", true, true, ""},      // auto restarts, then waits
	} {
		restart, err := applyDecision(c.policy, c.wait, c.required, c.state, c.manager)
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		if restart != c.restart || (c.err == "") != (err == nil) || (c.err != "" && !strings.Contains(msg, c.err)) {
			t.Errorf("%+v: restart %v err %q", c, restart, msg)
		}
	}
}

// A database that can't be read isn't one with nothing in it: its settings aren't the defaults, and a
// command that shows settings says it couldn't read them.
func TestUnreadableSettingsAreNotShownAsDefaults(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	root := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	db := filepath.Join(root, "local-base.db")
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(db + suffix) // only there if a process left them
	}
	if err := os.WriteFile(db, []byte("this isn't a database, and is long enough that SQLite reads it as a header to reject"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"doctor"}, {"models", "show", "qwen3.5-4b-q4_k_m"}} {
		code, out := runApp(t, append([]string{"--root", root}, args...)...)
		if code == 0 {
			t.Errorf("%v: exit 0, showing %.300q", args, out)
		}
		if !strings.Contains(out, "Could not read") && !strings.Contains(out, "never finished") {
			t.Errorf("%v: %.300q", args, out)
		}
		if strings.Contains(out, "Parallel Slots: auto") {
			t.Errorf("%v shows the defaults as this root's: %.300q", args, out)
		}
	}
}
