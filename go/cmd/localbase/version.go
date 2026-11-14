package main

import (
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/upgrade"
	"github.com/timsexperiments/LocalBase/go/internal/version"
)

// versionCommand says exactly which build this is. Bare --version stays the
// number alone, for scripts.
func versionCommand() *cli.Command {
	return &cli.Command{Name: "version", Summary: "Show this build's version, commit, date and Go version",
		Run: func(c *cli.Ctx) (any, error) {
			b := version.Get()
			if !c.Global.JSON {
				c.Info("LocalBase %s", b)
				if upgrade.BuiltWithTestKey() { // never a release: DESIGN §33.5
					c.Info("Built with the test signing key: not for release.")
				}
			}
			return b, nil
		}}
}
