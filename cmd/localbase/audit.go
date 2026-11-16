package main

import (
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/root"
)

// sayIfInstalled says what's about to happen when it's the installed service, the one on the default
// root, in words, before it happens. Anyone reading a script's output should be able to see it.
func sayIfInstalled(c *cli.Ctx, dir, what string) {
	if root.IsDefault(dir) {
		c.Error("%s", what)
	}
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
