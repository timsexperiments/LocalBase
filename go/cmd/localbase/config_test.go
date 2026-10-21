package main

import (
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
