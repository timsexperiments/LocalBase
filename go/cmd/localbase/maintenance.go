package main

import (
	"context"
	"os"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/conf"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/service"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

func maintenanceCommands() []*cli.Command {
	yes := cli.Option{Name: "yes", Alias: "y", Bool: true}
	return []*cli.Command{
		{Name: "reset", Summary: "Reset the LocalBase configuration database", Options: []cli.Option{withHelp(yes, "Confirm reset")},
			Run: func(c *cli.Ctx) (any, error) {
				if !c.Bool("yes") {
					return nil, cli.Inputf("reset is destructive. Re-run with --yes to confirm.")
				}
				r, err := root.Open(root.Resolve(c.Global.Root))
				if err != nil {
					return nil, err
				}
				lease, err := root.AcquireOperation(r.Dir, "reset")
				if err != nil {
					return nil, err
				}
				defer lease.Release()
				if service.HasEvidence(r) {
					if _, err := service.StopWithin(r); err != nil {
						return nil, err
					}
				}
				for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
					if err := os.Remove(r.DB() + suffix); err != nil && !os.IsNotExist(err) {
						return nil, err
					}
				}
				st, err := store.Open(r.DB())
				if err != nil {
					return nil, err
				}
				defer st.Close()
				cat, err := catalog.LoadRoot(r.Dir, nil)
				if err != nil {
					return nil, err
				}
				h := memory.NewHostProvider()
				cfg := conf.Default(r.Dir, int(float64(h.Topology().SystemCapacity)/float64(memory.GiB)+0.5), cat)
				if err := conf.Check(cfg, cat); err != nil {
					return nil, err
				}
				if err := st.SaveConfig(context.Background(), cfg); err != nil {
					return nil, err
				}
				c.Info("Database reset complete at %s", r.Dir)
				c.Info("Initialized default configuration.")
				return map[string]any{"reset": true, "root": r.Dir}, nil
			}},
		{Name: "uninstall", Summary: "Remove all LocalBase-managed data", Options: []cli.Option{withHelp(yes, "Confirm uninstall")},
			Run: func(c *cli.Ctx) (any, error) {
				if !c.Bool("yes") {
					return nil, cli.Inputf("uninstall removes all managed data. Re-run with --yes to confirm.")
				}
				r, err := root.Open(root.Resolve(c.Global.Root))
				if err != nil {
					return nil, err
				}
				lease, err := root.AcquireOperation(r.Dir, "uninstall")
				if err != nil {
					return nil, err
				}
				defer lease.Release()
				removed := false
				if service.HasEvidence(r) {
					in, err := service.Remove(r)
					if err != nil {
						return nil, err
					}
					c.Info("Removed LocalBase %s service definition.", in.Service.Manager)
					removed = true
				}
				if !removed {
					c.Info("No managed user service was installed.")
				}
				if err := os.RemoveAll(r.Dir); err != nil {
					return nil, err
				}
				c.Info("Removed all local-base managed data at %s", r.Dir)
				return map[string]any{"removed": true, "root": r.Dir}, nil
			}},
	}
}

func withHelp(o cli.Option, help string) cli.Option {
	o.Help = help
	return o
}
