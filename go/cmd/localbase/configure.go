package main

import "github.com/timsexperiments/LocalBase/go/internal/cli"

func configureCommand() *cli.Command {
	return &cli.Command{Name: "configure", Summary: "Configure models, ports, settings, and API keys",
		Run: func(c *cli.Ctx) (any, error) { return nil, cli.Inputf("configure is not implemented yet") }}
}
