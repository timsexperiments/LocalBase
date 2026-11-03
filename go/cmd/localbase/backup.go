package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/backup"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/version"
)

func backupCommand() *cli.Command {
	return &cli.Command{Name: "backup", Summary: "Write this root's settings to one private file",
		Options: []cli.Option{
			{Name: "output", Value: "<file>", Help: "Where to write it (default: beside the root, in a private directory)"},
			{Name: "exclude-secrets", Bool: true, Help: "Leave out tokens, client secrets, the SMTP password and download credentials"},
			{Name: "dry-run", Bool: true, Help: "Say what it would hold, and write nothing"},
		},
		Examples: []string{"local-base backup", "local-base backup --dry-run", "local-base backup --exclude-secrets --output ~/local-base.lbbackup"},
		Run:      runBackup}
}

func restoreCommand() *cli.Command {
	return &cli.Command{Name: "restore", Summary: "Restore this root's settings from a backup", Usage: "<file>", MaxArgs: 1,
		Options: []cli.Option{
			{Name: "dry-run", Bool: true, Help: "Check the file and say what would change, and change nothing"},
			{Name: "replace", Bool: true, Help: "Replace the settings of a root that has data. Its models stay, and its old files are kept"},
			{Name: "yes", Alias: "y", Bool: true, Help: "Do not ask"},
			{Name: "undo", Bool: true, Help: "Put back a root that a restore left unfinished"},
			{Name: "port-probe", Bool: true, Help: "Connect to the restored ports to see which are in use (on by default; --no-port-probe skips it)"},
		},
		Examples: []string{"local-base --root ~/restored restore --dry-run backup.lbbackup", "local-base --root ~/restored restore backup.lbbackup"},
		Run:      runRestore}
}

func runBackup(c *cli.Ctx) (any, error) {
	r, err := root.Open(root.Resolve(c.Global.Root))
	if err != nil {
		return nil, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var res *backup.Result
	if err := root.WithOperation(r.Dir, "backup", func() (err error) {
		res, err = backup.Create(ctx, backup.Options{Root: r, Output: c.String("output"), ExcludeSecrets: c.Bool("exclude-secrets"),
			DryRun: c.Bool("dry-run"), Version: version.Version, Now: time.Now()})
		return err
	}); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, errors.New("Interrupted. Nothing was kept, and its working files are gone.")
		}
		return nil, err
	}
	// On stderr, so it is seen even when the output goes somewhere else.
	fmt.Fprintln(c.Stderr, res.Warning)
	for _, caution := range res.Cautions {
		fmt.Fprintln(c.Stderr, caution)
	}
	if res.DryRun {
		c.Info("Dry run: nothing was written. It would go to %s.", res.Output)
	} else {
		c.Info("Wrote %s (%s), private to you.", res.Output, sizeText(res.Bytes))
	}
	printManifest(c, res.Manifest)
	if !res.DryRun {
		c.Info("To restore it: local-base --root <directory> restore %s", res.Output)
		auditCLI(&env{Root: r}, "backup.created", "event", "backup.created", "category", "audit", "component", "backup",
			"secretsIncluded", len(res.Manifest.SecretNames) > 0, "bytes", res.Bytes)
	}
	return res, nil
}

func sizeText(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

func printManifest(c *cli.Ctx, m *backup.Manifest) {
	c.Info("Holds:")
	for _, f := range m.Files {
		c.Info("  %s (%s)", f.Path, sizeText(f.Size))
	}
	if len(m.SecretsRemoved) > 0 {
		c.Info("Secrets left out, and the command that puts each back:")
		for _, s := range m.SecretsRemoved {
			c.Info("  %s: %s", s.Name, s.Restore)
		}
	}
	if len(m.NotIncluded) > 0 {
		c.Info("Not included:")
		for _, n := range m.NotIncluded {
			why := n.Why
			if n.Unknown {
				why = "unknown. " + why
			}
			c.Info("  %s (%s): %s", n.What, sizeText(n.Bytes), why)
		}
	}
	if len(m.RowsLeftOut) > 0 {
		c.Info("Rows left out: %s", strings.Join(m.RowsLeftOut, ", "))
	}
}

func runRestore(c *cli.Ctx) (any, error) {
	target := root.Resolve(c.Global.Root)
	if c.Bool("undo") {
		if err := backup.Undo(target); err != nil {
			return nil, err
		}
		c.Info("The unfinished restore on %s was undone.", target)
		return map[string]any{"undone": true}, nil
	}
	if len(c.Args) != 1 {
		return nil, cli.Inputf("restore needs the backup file: local-base restore <file>")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	s, err := backup.Stage(ctx, c.Args[0], target, c.Bool("replace"))
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, errors.New("Interrupted. Nothing was changed, and its working files are gone.")
		}
		return nil, err
	}
	defer s.Discard()
	rep := s.Report
	if probe, given := c.OptBool("port-probe"); given && !probe {
		rep.Cautions = append(rep.Cautions, fmt.Sprintf("Ports were not checked. These settings name %v, so make sure they are free before starting this root.", rep.Ports))
	} else {
		for _, port := range rep.Ports {
			if portListening(port) {
				rep.Cautions = append(rep.Cautions, fmt.Sprintf("Something already uses port %d, which these settings name. Choose free ports before starting this root: local-base --root '%s' config apply --file <file>", port, rep.Target))
			}
		}
	}
	printRestoreReport(c, rep)
	if c.Bool("dry-run") {
		c.Info("Dry run: nothing was changed. A real restore would proceed.")
		return map[string]any{"dryRun": true, "report": rep}, nil
	}
	if !c.Bool("yes") {
		const needYes = "restore replaces settings. Re-run with --yes to confirm."
		if c.Global.JSON || c.Global.NonInteractive || !isTerminal(os.Stdin) {
			return nil, cli.Inputf(needYes)
		}
		fmt.Fprint(c.Stderr, "Continue? [y/N] ")
		// In its own goroutine: an interrupt at this question must still end the run and remove
		// the working files, and a read of the terminal can't be interrupted.
		type answer struct {
			line string
			err  error
		}
		answered := make(chan answer, 1)
		go func() {
			line, err := bufio.NewReader(os.Stdin).ReadString('\n')
			answered <- answer{line, err}
		}()
		select {
		case <-ctx.Done():
			return nil, errors.New("Interrupted. Nothing was changed, and its working files are gone.")
		case a := <-answered:
			if a.err != nil && a.line == "" {
				return nil, cli.Inputf(needYes)
			}
			if v := strings.ToLower(strings.TrimSpace(a.line)); v != "y" && v != "yes" {
				return nil, errors.New("Stopped. Nothing was changed.")
			}
		}
	}
	done, err := s.Commit()
	if err != nil {
		return nil, err
	}
	if r, err := root.Open(rep.Target); err == nil {
		auditCLI(&env{Root: r}, "backup.restored", "event", "backup.restored", "category", "audit", "component", "backup",
			"secretsIncluded", len(rep.Manifest.SecretNames) > 0, "revocationsKept", rep.RevocationsKept)
	}
	printRestoreNext(c, rep, done)
	return map[string]any{"report": rep, "beforeDir": done.BeforeDir}, nil
}

func printRestoreReport(c *cli.Ctx, rep *backup.Report) {
	if rep.NewRoot {
		c.Info("This makes a new root at %s.", rep.Target)
	} else {
		c.Info("This replaces the settings of %s. Its models stay, and its old files are kept.", rep.Target)
	}
	m := rep.Manifest
	c.Info("The backup was made %s by version %s. It holds %d keys and %d users.", m.CreatedAt, m.Version, rep.Keys, rep.Users)
	if len(m.SecretNames) > 0 {
		c.Info("Secrets in it: %s.", strings.Join(m.SecretNames, ", "))
	} else {
		c.Info("It holds no secrets.")
	}
	for _, rule := range rep.Rules {
		c.Info("%s", rule)
	}
	if rep.RevocationsKept > 0 {
		c.Info("%d keys revoked on this root stay revoked.", rep.RevocationsKept)
	}
	if len(rep.ReturningKeys) > 0 {
		c.Info("Keys this root no longer has would work again: %s.", strings.Join(rep.ReturningKeys, ", "))
	}
	if len(rep.KeysLost) > 0 {
		c.Info("Keys made since this backup would stop working: %s.", strings.Join(rep.KeysLost, ", "))
	}
	if rep.ReturningUsers > 0 {
		c.Info("%d users this root has removed would come back.", rep.ReturningUsers)
	}
	if rep.UserStatusesKept > 0 {
		c.Info("%d users removed or disabled on this root stay that way.", rep.UserStatusesKept)
	}
	if rep.UsersChanged > 0 {
		c.Info("%d users would go back to the status they had when this backup was made.", rep.UsersChanged)
	}
	if len(rep.Programs) > 0 {
		c.Info("These commands run as you when their model loads:")
		for _, p := range rep.Programs {
			line := "  " + p.Backend + ": " + strings.Join(p.Command, " ")
			if !p.Exists {
				line += " (not on this computer)"
			}
			c.Info("%s", line)
		}
	}
	if len(rep.Sources) > 0 {
		c.Info("Imported models that reach outside the catalog:")
		for _, s := range rep.Sources {
			c.Info("  %s %s %s", s.Model, s.Kind, s.Where)
		}
	}
	if len(rep.Providers) > 0 {
		c.Info("Sign-in: %s.", strings.Join(rep.Providers, ", "))
	} else {
		c.Info("Sign-in: on this computer only, until a provider is added.")
	}
	if len(rep.Settings) > 0 {
		c.Info("Settings that change who can reach it, or where it reaches:")
		for _, setting := range rep.Settings {
			c.Info("  %s: %s", setting.Name, setting.Value)
		}
	}
	for _, caution := range rep.Cautions {
		fmt.Fprintln(c.Stderr, caution)
	}
}

func printRestoreNext(c *cli.Ctx, rep *backup.Report, done *backup.Done) {
	flag := "local-base --root '" + rep.Target + "'"
	c.Info("Restored to %s. Nothing was started.", rep.Target)
	c.Info("Next:")
	c.Info("  Start it: %s start", flag)
	if len(rep.Providers) == 0 {
		c.Info("  Everyone must sign in again. For a sign-in link: %s access login-link", flag)
	} else {
		c.Info("  Everyone must sign in again.")
	}
	if len(rep.Manifest.Models) > 0 {
		c.Info("  Install any of these it doesn't have: %s", strings.Join(rep.Manifest.Models, ", "))
		c.Info("    %s models install <id>", flag)
	}
	for _, s := range rep.Manifest.SecretsRemoved {
		c.Info("  Put back %s: %s", s.Name, s.Restore)
	}
	// Last, so it isn't missed. These files hold secrets.
	if done.BeforeDir != "" {
		c.Info("The old files are in %s. They hold secrets. When you no longer need them: rm -rf '%s'", done.BeforeDir, done.BeforeDir)
	}
}

// backupsLeftBehind names a root's backups directory, if it holds anything, for uninstall.
func backupsLeftBehind(rootDir string) string {
	dir := backup.DefaultDir(rootDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) || err != nil || len(entries) == 0 {
		return ""
	}
	return dir
}
