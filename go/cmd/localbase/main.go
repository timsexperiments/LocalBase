// Command localbase is the LocalBase gateway and CLI, installed as
// `local-base`. Commands follow the TypeScript CLI's names, options and JSON
// envelopes; see go/DESIGN.md §5.
package main

import (
	"os"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/gateway"
)

func main() {
	os.Exit(app().Run(os.Args[1:]))
}

func app() *cli.App {
	// The TS command order.
	commands := []*cli.Command{initCommand(), configureCommand(), doctorCommand(), modelsCommand(), serveCommand()}
	commands = append(commands, serviceCommands()...)
	commands = append(commands, logsCommand(), smokeCommand(), keysCommand(), accessCommand(), backendsCommand(), authProfilesCommand(), voicesCommand(), speechCommand())
	commands = append(commands, maintenanceCommands()...)
	return &cli.App{
		Name:    "local-base",
		Version: gateway.Version,
		Summary: "Local AI installer, manager, and OpenAI-compatible gateway",
		Default: "configure",
		Root:    &cli.Command{Sub: commands},
	}
}
