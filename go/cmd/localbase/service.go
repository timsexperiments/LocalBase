package main

import (
	"fmt"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/service"
)

func printStatus(c *cli.Ctx, in service.Inspection) {
	s, g := in.Service, in.Gateway
	c.Info("Service: %s (%s)", s.State, s.Manager)
	mgr := "unavailable"
	if s.ManagerAvailable {
		mgr = "unknown"
		if s.ManagerState != nil {
			mgr = *s.ManagerState
		}
	}
	c.Info("Manager: %s", mgr)
	def := "not installed"
	if s.DefinitionInstalled {
		def = "installed"
	}
	c.Info("Definition: %s (%s)", def, s.DefinitionPath)
	proc := "not running"
	if s.PID != nil {
		proc = fmt.Sprintf("pid %d", *s.PID)
		if s.UptimeSeconds != nil {
			proc += fmt.Sprintf(", uptime %ds", *s.UptimeSeconds)
		}
	}
	c.Info("Process: %s", proc)
	restarts := "unavailable"
	if s.RestartCount != nil {
		restarts = fmt.Sprint(*s.RestartCount)
	}
	c.Info("Restarts: %s", restarts)
	gw := g.State
	if g.State != "ready" {
		gw += " (" + g.Detail + ")"
	}
	c.Info("Gateway: %s", gw)
}

func serviceCommands() []*cli.Command {
	installMissing := cli.Option{Name: "install-missing", Bool: true, Help: "Allow the service to download selected models"}
	managed := func(restart bool) func(c *cli.Ctx) (any, error) {
		return func(c *cli.Ctx) (any, error) {
			e, err := openEnv(c)
			if err != nil {
				return nil, err
			}
			defer e.Close()
			if c.Bool("install-missing") && !e.Config.InstallMissingModels {
				cfg := e.Config.Clone()
				cfg.InstallMissingModels = true
				if err := e.save(cfg); err != nil {
					return nil, err
				}
			}
			in, err := service.Start(e.Root, restart, c.Bool("install-missing"))
			if err != nil {
				return nil, err
			}
			action, verb := "start", "Started"
			if restart {
				action, verb = "restart", "Restarted"
			}
			if in.Service.State != "running" && in.Service.State != "starting" {
				return nil, fmt.Errorf("LocalBase service manager reported %s after %s.", in.Service.State, action)
			}
			c.Info("%s LocalBase service with %s.", verb, in.Service.Manager)
			printStatus(c, in)
			return in, nil
		}
	}
	return []*cli.Command{
		{Name: "start", Summary: "Install, enable, and start the LocalBase user service", Options: []cli.Option{installMissing}, Run: managed(false)},
		{Name: "stop", Summary: "Stop and disable the LocalBase user service", Run: func(c *cli.Ctx) (any, error) {
			e, err := openEnv(c)
			if err != nil {
				return nil, err
			}
			defer e.Close()
			in, err := service.Stop(e.Root)
			if err != nil {
				return nil, err
			}
			c.Info("Stopped and disabled LocalBase service managed by %s.", in.Service.Manager)
			return in, nil
		}},
		{Name: "restart", Summary: "Refresh, enable, and restart the LocalBase user service", Options: []cli.Option{installMissing}, Run: managed(true)},
		{Name: "status", Summary: "Show LocalBase service and gateway readiness", Run: func(c *cli.Ctx) (any, error) {
			e, err := openEnv(c)
			if err != nil {
				return nil, err
			}
			defer e.Close()
			var in service.Inspection
			if err := root.WithOperation(e.Root.Dir, "status", func() (err error) {
				in, err = service.Inspect(e.Root, false)
				return err
			}); err != nil {
				return nil, err
			}
			printStatus(c, in)
			return in, nil
		}},
	}
}
