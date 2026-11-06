package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
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
	return &cli.Command{Name: "restore", Summary: "Restore this root's settings from a backup you made", Usage: "<file>", MaxArgs: 1,
		Options: []cli.Option{
			{Name: "dry-run", Bool: true, Help: "Check the file and print its report, and change nothing. The report doesn't show everything a file can change"},
			{Name: "replace", Bool: true, Help: "Replace the settings of a root that has data. Its models stay, and its old files are kept"},
			{Name: "bring-back-key", Value: "<id>", Repeatable: true, Help: "Take this backup's version of a key the report says is held back, stays out, or stays as it is on this root"},
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
	c.Error("%s", res.Warning)
	for _, caution := range res.Cautions {
		c.Error("%s", caution)
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
	printIssuers(c, m.Issuers)
	if n := m.Unbound.Total(); n > 0 {
		c.Info("%d email bindings and invitations name no sign-in provider.", n)
	}
}

// printKeys lists keys by the id that identifies them and the name a person gave them. Past the cap
// it says how many more, as the list of who has a role does.
func printKeys(c *cli.Ctx, heading, how string, keys []backup.KeyChange) {
	if len(keys) == 0 {
		return
	}
	c.Info("%s", heading)
	for _, k := range keys[:min(len(keys), maxGrantLines)] {
		line := "  " + k.ID + " (" + k.Name + ")"
		if k.Why != "" {
			line += ": " + k.Why
		}
		c.Info("%s", line)
	}
	if hidden := len(keys) - maxGrantLines; hidden > 0 {
		c.Info("  and %d more. All of them: restore --dry-run --json", hidden)
	}
	if how != "" {
		c.Info("%s", how)
	}
}

// plainKeyID is the shape of an id that's safe to offer for pasting into a shell. An id comes from the
// file, so a line made to be copied can't carry one a shell would act on, or take for an option.
var plainKeyID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)

// bringBackFlag is a key's flag ready to copy, or the form to fill in if its id isn't safe to paste.
func bringBackFlag(id string) string {
	if plainKeyID.MatchString(id) {
		return "--bring-back-key " + id
	}
	return "--bring-back-key <id>"
}

// printHeldBack gives each key its own line to bring it back, so that a key comes back because someone
// picked it. One line for all of them would bring back whatever a file planted among them.
func printHeldBack(c *cli.Ctx, keys []backup.KeyChange) {
	if len(keys) == 0 {
		return
	}
	c.Info("Keys held back, because they can do more than make requests. Bring back only one you recognise:")
	for _, k := range keys[:min(len(keys), maxGrantLines)] {
		c.Info("  %s (%s): %s", k.ID, k.Name, k.Why)
		c.Info("    To bring it back: %s", bringBackFlag(k.ID))
	}
	if hidden := len(keys) - maxGrantLines; hidden > 0 {
		c.Info("  and %d more. All of them: restore --dry-run --json", hidden)
	}
}

// shellQuoted makes a path one word in a line made to be pasted, whatever it holds.
func shellQuoted(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// printIssuers says who each sign-in provider's bindings cover. They decide who gets which role.
func printIssuers(c *cli.Ctx, lines []backup.IssuerLine) {
	if len(lines) == 0 {
		return
	}
	c.Info("Who gets a role, by sign-in provider:")
	for _, l := range lines {
		state := "active"
		if !l.Active {
			state = "inactive until this provider is added back"
		}
		c.Info("  %s: %d role bindings, %d invitations, %s", l.Issuer, l.Bindings, l.Invitations, state)
	}
}

func keyIDs(keys []backup.KeyChange) string {
	ids := make([]string, len(keys))
	for i, k := range keys {
		ids[i] = k.ID
	}
	return strings.Join(ids, ",")
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
	s, err := backup.Stage(ctx, c.Args[0], target, c.Bool("replace"), c.Strings("bring-back-key")...)
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
			"secretsIncluded", len(rep.Manifest.SecretNames) > 0, "revocationsKept", rep.RevocationsKept,
			"keysKept", len(rep.KeysKept), "keysLeftOut", len(rep.KeysLeftOut), "keysHeldBack", len(rep.KeysHeldBack), "keysBroughtBack", keyIDs(rep.KeysBroughtBack))
	}
	printRestoreNext(c, rep, done)
	return map[string]any{"report": rep, "beforeDir": done.BeforeDir}, nil
}

// printable is a copy of the report that's safe to show on a terminal. Every string in it, wherever
// it came from, has been through cli.PrintableASCII, because a person judges what's in this report by
// eye, and a letter from another alphabet can make one address or program look like another. Only a
// field tagged report:"letters", which is a name a person gave or the path they typed, keeps its
// letters, through cli.Printable. Doing it here, once, means a field added to the report can't be
// missed at the line that prints it. The report itself stays as it is for --json. A field of a kind
// this doesn't know stops it, and TestEveryStringInTheReportIsMadePrintable finds that first.
func printable(rep *backup.Report) *backup.Report {
	out := &backup.Report{}
	copyPrintable(reflect.ValueOf(out).Elem(), reflect.ValueOf(rep).Elem(), false)
	return out
}

func copyPrintable(dst, src reflect.Value, letters bool) {
	switch src.Kind() {
	case reflect.String:
		if letters {
			dst.SetString(cli.Printable(src.String()))
		} else {
			dst.SetString(cli.PrintableASCII(src.String()))
		}
	case reflect.Struct:
		for i := range src.NumField() {
			copyPrintable(dst.Field(i), src.Field(i), letters || src.Type().Field(i).Tag.Get("report") == "letters")
		}
	case reflect.Slice:
		if !src.IsNil() {
			dst.Set(reflect.MakeSlice(src.Type(), src.Len(), src.Len()))
			for i := range src.Len() {
				copyPrintable(dst.Index(i), src.Index(i), letters)
			}
		}
	case reflect.Pointer:
		if !src.IsNil() {
			dst.Set(reflect.New(src.Type().Elem()))
			copyPrintable(dst.Elem(), src.Elem(), letters)
		}
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		dst.Set(src)
	default:
		panic("the restore report has a field of a kind that can't be made printable: " + src.Type().String())
	}
}

func printRestoreReport(c *cli.Ctx, rep *backup.Report) {
	rep = printable(rep)
	if rep.NewRoot {
		c.Info("This makes a new root at %s.", rep.Target)
	} else {
		c.Info("This replaces the settings of %s. Its models stay, and its old files are kept.", rep.Target)
	}
	m := rep.Manifest
	keys := fmt.Sprintf("%d active, %d revoked", rep.Keys.Active, rep.Keys.Revoked)
	if rep.Keys.Expired > 0 {
		keys += fmt.Sprintf(", %d expired", rep.Keys.Expired)
	}
	c.Info("The backup was made %s by version %s. It holds %d API keys (%s) and %d users.", m.CreatedAt, m.Version, rep.Keys.Total, keys, rep.Users)
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
	printKeys(c, "API keys in this backup that would work:", "", rep.KeysActive)
	printHeldBack(c, rep.KeysHeldBack)
	printKeys(c, "Keys changed since this backup stay as they are on this root:", "  To take this backup's version of one: --bring-back-key <id>", rep.KeysKept)
	printKeys(c, "Keys in this backup that this root doesn't have stay out:", "  To bring one back: --bring-back-key <id>", rep.KeysLeftOut)
	printKeys(c, "Keys going back to what this backup holds, because you named them:", "  This backup's secret for each of these will work. The secret each has now will stop working.", rep.KeysBroughtBack)
	printKeys(c, "Keys made since this backup would stop working:", "", rep.KeysLost)
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
	printSignIn(c, rep.SignIn)
	printIssuers(c, rep.Issuers)
	printGrants(c, rep.Grants, rep.RolesGone)
	if rep.UnboundNote != "" {
		c.Info("%s", rep.UnboundNote)
	}
	for _, l := range rep.ReturningBindings {
		c.Info("Role bindings this root no longer has would come back: %s: %d.", l.Issuer, l.Bindings)
	}
	if len(rep.Settings) > 0 {
		c.Info("Settings that change who can reach it, or where it reaches:")
		for _, setting := range rep.Settings {
			c.Info("  %s: %s", setting.Name, setting.Value)
		}
	}
	for _, caution := range rep.Cautions {
		c.Error("%s", caution)
	}
}

// printSignIn says who could sign in and what they'd get. The line about what everyone gets is always
// there, because a missing one would read as safe.
func printSignIn(c *cli.Ctx, s *backup.SignIn) {
	if s == nil {
		return
	}
	if len(s.Who) > 0 {
		c.Info("Who can sign in:")
		for _, l := range s.Who {
			c.Info("  %s", l.Text("has"))
		}
	} else {
		c.Info("Sign-in: on this computer only, until a provider is added.")
	}
	for _, l := range s.Goes {
		c.Info("No longer able to sign in: %s (this root names it %s)", l.Value, l.Named)
	}
	everyone := s.Everyone
	if everyone.Value == "" {
		everyone.Value = "nothing, unless a role gives it"
	}
	c.Info("Everyone who signs in through a provider gets: %s", everyone.Text("gives"))
	if s.IfPolicyRemoved != "" {
		c.Info("If the access policy is removed, everyone who signs in through a provider gets: %s", s.IfPolicyRemoved)
	}
	if s.Origin != nil {
		c.Info("Address people sign in at: %s", s.Origin.Text("has"))
	}
	if s.Mail != nil {
		c.Info("Mail server, which can read sign-in links: %s", s.Mail.Text("has"))
	}
}

// maxGrantLines is how many people a role lists before saying how many more. Those new to this root
// come first, so this can't bury them, and --json has them all.
const maxGrantLines = 50

// printGrants says what each role gives and who has it, by name. Roles the root would lose come first.
func printGrants(c *cli.Ctx, grants, gone []backup.Grant) {
	if len(grants)+len(gone) == 0 {
		return
	}
	c.Info("What each role gives, and who has it:")
	for _, g := range gone {
		gives := g.Gives.Value
		if gives == "" {
			gives = "nothing"
		}
		c.Info("  No longer a role here: %s (this root gives %s)", g.Role, gives)
		printWho(c, g.Who)
	}
	for _, g := range grants {
		gives := g.Gives
		if gives.Value == "" {
			gives.Value = "nothing"
		}
		line := gives.Text("gives")
		if g.Default {
			line += " (everyone who signs in through a provider has this)"
		}
		c.Info("  %s: %s", g.Role, line)
		switch {
		case len(g.Who) == 0 && g.Default:
			c.Info("    nobody else")
		case len(g.Who) == 0:
			c.Info("    nobody")
		}
		printWho(c, g.Who)
	}
}

// printWho lists who has a role, up to the cap, and says how many it didn't print and what's changing
// for them.
func printWho(c *cli.Ctx, who []backup.Line) {
	for _, l := range who[:min(len(who), maxGrantLines)] {
		if l.Against == backup.AgainstGone {
			c.Info("    no longer has this role: %s", l.Value)
			continue
		}
		c.Info("    %s", l.Text("has"))
	}
	hidden := who[min(len(who), maxGrantLines):]
	if len(hidden) == 0 {
		return
	}
	newOnes, leaving := 0, 0
	for _, l := range hidden {
		switch l.Against {
		case backup.AgainstNew:
			newOnes++
		case backup.AgainstGone:
			leaving++
		}
	}
	more := fmt.Sprintf("and %d more", len(hidden))
	if newOnes > 0 {
		more += fmt.Sprintf(", %d of them new", newOnes)
	}
	if leaving > 0 {
		more += fmt.Sprintf(", %d of them losing this role", leaving)
	}
	c.Info("    %s. All of them: restore --dry-run --json", more)
}

// backupArgument is the file this restore was given, as a word to paste from any folder.
func backupArgument(c *cli.Ctx) string {
	if len(c.Args) == 0 {
		return "<this backup>"
	}
	file := c.Args[0]
	if abs, err := filepath.Abs(file); err == nil {
		file = abs
	}
	return shellQuoted(cli.Printable(file))
}

func printRestoreNext(c *cli.Ctx, rep *backup.Report, done *backup.Done) {
	rep = printable(rep)
	flag := "local-base --root " + shellQuoted(rep.Target)
	c.Info("Restored to %s. Nothing was started.", rep.Target)
	c.Info("Next:")
	c.Info("  Start it: %s start", flag)
	if len(rep.KeysHeldBack) > 0 {
		name := "--bring-back-key <id>"
		if len(rep.KeysHeldBack) == 1 {
			name = bringBackFlag(rep.KeysHeldBack[0].ID)
		}
		c.Info("  A key held back can still come back from this backup: %s restore --replace %s %s", flag, name, backupArgument(c))
	}
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
		before := cli.Printable(done.BeforeDir)
		c.Info("The old files are in %s. They hold secrets. When you no longer need them: rm -rf %s", before, shellQuoted(before))
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
