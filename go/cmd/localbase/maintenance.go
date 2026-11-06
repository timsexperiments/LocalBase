package main

import (
	"context"
	"fmt"
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
					sayIfInstalled(c, r.Dir, "This stops the installed LocalBase service.")
					_, err := service.StopWithin(r)
					auditService(&env{Root: r, warnings: c.Stderr}, "audit.service-stopped", err, "purpose", "reset", "disabled", err == nil)
					if err != nil {
						return nil, err
					}
				}
				for _, suffix := range append([]string{""}, store.Sidecars...) {
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
				e := &env{Root: r, warnings: c.Stderr}
				auditChange(e, "config", "reset", "configuration", "")
				c.Info("Database reset complete at %s", r.Dir)
				c.Info("Initialized default configuration.")
				return e.withAuditResult(map[string]any{"reset": true, "root": r.Dir}), nil
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
				// The last record, and it goes with the logs, so it's on the screen as well.
				audited := &env{Root: r, warnings: c.Stderr}
				sayIfInstalled(c, r.Dir, "This stops and removes the installed LocalBase service.")
				auditChange(audited, "root", "uninstall", "root", "", "root", r.Dir, "service_evidence", service.HasEvidence(r))
				c.Error("Uninstalling LocalBase at %s. Its audit log goes with it.", r.Dir)
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
				// Found while the root's name still resolves, and gone before the root is.
				leftCopies, readingCopies := store.ReadCopies(r.Dir)
				copiesRemoved := 0
				var copyFailures []string
				for _, left := range leftCopies {
					if err := store.RemoveReadCopy(left); err != nil {
						shown := shownPath(left.Path)
						copyFailures = append(copyFailures, fmt.Sprintf("Could not remove a private copy of this root's database at %s: %s. It may hold secrets. To delete it: rm -rf %s", shown, cli.Printable(err.Error()), shown))
						continue
					}
					copiesRemoved++
				}
				if err := os.RemoveAll(r.Dir); err != nil {
					return nil, err
				}
				c.Info("Removed all local-base managed data at %s", r.Dir)
				out := map[string]any{"removed": true, "root": r.Dir, "readCopiesRemoved": copiesRemoved}
				switch {
				case copiesRemoved == 1:
					c.Info("Removed 1 private copy of this root's database left by an interrupted read.")
				case copiesRemoved > 1:
					c.Info("Removed %d private copies of this root's database left by interrupted reads.", copiesRemoved)
				}
				for _, failure := range copyFailures {
					c.Info("%s", failure)
				}
				for _, reading := range readingCopies {
					c.Info("A LocalBase command still reading this root has a private copy at %s. It's left on purpose: that command removes it when it ends.", shownPath(reading.Path))
				}
				if dir := backupsLeftBehind(r.Dir); dir != "" {
					c.Info("Backups in %s were left behind. They hold secrets. To delete them: rm -rf '%s'", dir, dir)
					out["backupsLeftBehind"] = dir
				}
				return out, nil
			}},
	}
}

func withHelp(o cli.Option, help string) cli.Option {
	o.Help = help
	return o
}

// shownPath is a path as a person reads it and pastes it: nothing in it acts on the terminal.
func shownPath(path string) string { return root.ShellQuote(cli.Printable(path)) }
