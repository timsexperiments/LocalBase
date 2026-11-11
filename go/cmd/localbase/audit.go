package main

import (
	"errors"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/upgrade"
)

// sayIfInstalled says what's about to happen when it's the installed service, the one on the default
// root, in words, before it happens. Anyone reading a script's output should be able to see it.
func sayIfInstalled(c *cli.Ctx, dir, what string) {
	if root.IsDefault(dir) {
		c.Error("%s", what)
	}
}

// versionForAudit is what a binary said its version is, if it is one: whatever else it says stays out.
func versionForAudit(v string) string {
	if !upgrade.Compare(v, "v0.0.0").Known {
		return "unknown"
	}
	return v
}

// upgradeOutcome is how an Apply ended, in words from a short list.
func upgradeOutcome(res upgrade.Result, err error) string {
	var smoke *upgrade.SmokeFailed
	switch {
	case err == nil:
		return "applied"
	case res.RolledBack:
		return "rolled-back"
	case errors.As(err, &smoke):
		return "smoke-failed"
	}
	return "failed"
}

// userAction is the route's word for what userCommand does, by the status it sets.
func userAction(status string) string {
	switch status {
	case "":
		return "remove-user"
	case "active":
		return "enable-user"
	}
	return "disable-user"
}
