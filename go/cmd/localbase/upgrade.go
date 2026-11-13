package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/install"
	"github.com/timsexperiments/LocalBase/go/internal/localhttp"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/service"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/termsafe"
	"github.com/timsexperiments/LocalBase/go/internal/upgrade"
	"github.com/timsexperiments/LocalBase/go/internal/version"
)

const (
	upgradeMaxWait = 600 // seconds
	// upgradeNeedsBytes is room for an archive and its binary, beside the installed one.
	upgradeNeedsBytes = 512 << 20
)

// upgradeReadyWait is how long a restarted gateway gets to answer ready.
const upgradeReadyWait = 90 * time.Second

// readyWaitLimit is upgradeReadyWait, or the shorter wait a test set.
func (e *env) readyWaitLimit() time.Duration {
	if e.readyWait > 0 {
		return e.readyWait
	}
	return upgradeReadyWait
}

func upgradeCommand() *cli.Command {
	return &cli.Command{Name: "upgrade", Summary: "Update this binary to a newer release",
		Options: []cli.Option{
			{Name: "check", Bool: true, Help: "Say whether a newer release exists, and do nothing else"},
			{Name: "version", Value: "<vX.Y.Z>", Help: "An exact release, instead of the latest"},
			{Name: "allow-downgrade", Bool: true, Help: "Allow installing an older release"},
			{Name: "yes", Alias: "y", Bool: true, Help: "Do not ask"},
			{Name: "wait", Value: "<seconds>", Help: "If the gateway is busy, wait for its work to finish, up to 600 s"},
			{Name: "force", Bool: true, Help: "If the gateway is busy, go ahead and cut its work"},
		},
		Examples: []string{"local-base upgrade --check", "local-base upgrade", "local-base upgrade --wait 120", "local-base upgrade rollback"},
		Run:      runUpgrade,
		Sub: []*cli.Command{
			{Name: "rollback", Summary: "Go back to the previous version", Run: withEnv(runUpgradeRollback)},
			{Name: "check-updates", Summary: "Turn the daily check for a newer version on or off", Usage: "<on|off>", MinArgs: 1, MaxArgs: 1, Run: withEnv(runCheckUpdates)},
		}}
}

// runUpgrade starts here because --check must not open or write anything.
func runUpgrade(c *cli.Ctx) (any, error) {
	if c.Bool("check") {
		return runUpgradeCheck(c)
	}
	e, err := openEnv(c)
	if err != nil {
		return nil, err
	}
	defer e.Close()
	return runUpgradeInstall(c, e)
}

func runUpgradeCheck(c *cli.Ctx) (any, error) {
	data := map[string]any{"current": version.Version, "release": version.ReleaseBuild()}
	if !version.ReleaseBuild() {
		c.Info("This is a development build (%s), not a release, so there is nothing to compare.", version.Version)
		return data, nil
	}
	latest, err := upgrade.DefaultSource().Latest(context.Background(), "")
	if err != nil {
		return nil, err
	}
	cmp := upgrade.Compare(version.Version, latest.Tag)
	data["latest"], data["url"], data["newer"], data["known"] = latest.Tag, latest.URL, cmp.Newer, cmp.Known
	switch {
	case !cmp.Known:
		c.Info("Current version: %s. GitHub's answer (%q) is not a LocalBase release, so there is nothing to compare.", version.Version, latest.Tag)
	case cmp.Newer:
		c.Info("A newer version is available: %s (this is %s).", latest.Tag, version.Version)
		c.Info("Release notes: %s", termsafe.PrintableASCII(latest.URL))
		c.Info("To install it: local-base upgrade")
	default:
		c.Info("This is up to date: %s (the latest release is %s).", version.Version, latest.Tag)
	}
	return data, nil
}

func upgradeBinaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// idleWaitBase is where the running gateway answers, proven to be this root's. The wait sends it
// a temporary key, so an owner record that names something else (a reused PID, another listener on
// the port) gets nothing. It is checked again here, though gatherGatewayFacts already proved it:
// time has passed, and the owner may be gone or changed. The refusal is upgrade.ClassifyGateway's.
func idleWaitBase(e *env) (string, error) {
	state, owner, detail := service.VerifiedOwnerState(e.Root)
	if state != "active" || owner == nil {
		if detail == "" {
			detail = "its lock could not be read"
		}
		return "", errors.New("Cannot tell whether a gateway is running on this root: " + detail + ". Nothing was changed.")
	}
	return strings.TrimSuffix(owner.HealthURL(), "/health"), nil
}

// waitGatewayIdle waits for the running gateway's work to finish (upgrade.WaitIdle), asking with one
// temporary key for the whole wait: created once, and deleted once at the end, each audited.
// Before, every poll made and deleted a key, up to 600 keys and 1,200 audit lines for one --wait.
func waitGatewayIdle(c *cli.Ctx, e *env, base string, limit time.Duration, sleep func(time.Duration)) error {
	ctx := context.Background()
	k, raw, end, err := e.audit.TemporaryKey(ctx, e.Store, "local-base upgrade (temporary)", []string{"models:read"}, "upgrade")
	if err != nil {
		return fmt.Errorf("Could not ask the gateway how busy it is: %w. Nothing was changed. --force goes ahead anyway.", err)
	}
	defer func() {
		if derr := end(); derr != nil {
			c.Error("Could not delete the temporary key %s: %v. It can only list models and it expires in a day. To remove it now: local-base keys revoke %s", k.ID, derr, k.ID)
		}
	}()
	return upgrade.WaitIdle(func() (bool, string, error) { return gatewayActivity(base, raw) }, limit, sleep)
}

// gatewayActivity asks the running gateway how busy it is, with the key it is given.
func gatewayActivity(base, raw string) (busy bool, detail string, err error) {
	req, err := http.NewRequest(http.MethodGet, base+"/_localbase/host", nil)
	if err != nil {
		return false, "", err
	}
	req.Header.Set("Authorization", "Bearer "+raw)
	resp, err := localhttp.New(5 * time.Second).Do(req) // the key goes to the gateway alone: no proxy, no redirect
	if err != nil {
		return false, "", err
	}
	defer resp.Body.Close()
	return upgrade.ParseActivity(resp.StatusCode, resp.Body)
}

// managedOps stop and restart through the service manager. Stopping is confirmed, because the
// sweep of §29 cannot cover this: a backend from before that version has no record for it.
func managedOps(c *cli.Ctx, e *env, binary string, arguments []string) upgrade.Ops {
	return upgrade.Ops{
		Stop: func() error {
			c.Info("Stopping the gateway...")
			_, err := service.Stop(e.Root)
			e.audit.Service("audit.service-stopped", err, "purpose", "upgrade", "disabled", err == nil)
			if err != nil {
				return err
			}
			return service.WaitStopped(e.Root, runtime.BackendPorts(e.Config, arguments), runtime.PortListening, 15*time.Second)
		},
		Start: func() error {
			c.Info("Restarting through the service manager...")
			if err := runSelf(binary, e.Root.Dir, "restart"); err != nil {
				return err
			}
			// restart returns once the manager has taken it, which can be before the
			// gateway listens. Smoke would then fail on a release that is fine, and a
			// gateway that never comes up has to roll back.
			if err := service.WaitReady(e.Root, e.readyWaitLimit()); err != nil {
				return fmt.Errorf("the gateway did not become ready: %w", err)
			}
			return nil
		},
		Smoke: func() error {
			c.Info("Running smoke...")
			return runSelf(binary, e.Root.Dir, "smoke")
		},
	}
}

// runSelf runs the installed binary, which after a swap is the new one.
func runSelf(binary, rootDir string, args ...string) error {
	cmd := exec.Command(binary, append([]string{"--root", rootDir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		text := strings.TrimSpace(string(out))
		if len(text) > 800 {
			text = text[len(text)-800:]
		}
		return fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, text)
	}
	return nil
}

func rollbackLine(binary string) string {
	return "mv " + binary + upgrade.PreviousSuffix + " " + binary + " && local-base restart"
}

func gatherGatewayFacts(e *env, binary string) (upgrade.GatewayFacts, []string) {
	// Proven, not only alive: whether the gateway is managed comes from its owner record, and the
	// upgrade sends what it learns here a key (idleWaitBase).
	state, owner, detail := service.VerifiedOwnerState(e.Root)
	f := upgrade.GatewayFacts{LockState: state, LockDetail: detail, Binary: binary, OwnerManaged: state == "active" && owner != nil && owner.ServiceID != ""}
	if state != "active" {
		return f, nil
	}
	if in, err := service.Inspect(e.Root, false); err == nil {
		f.DefinitionInstalled, f.Reachable = in.Service.DefinitionInstalled, in.Service.ManagerAvailable
	}
	program, arguments := service.InstalledInvocation(e.Root.Dir)
	f.ServiceProgram = program
	return f, arguments
}

func runUpgradeInstall(c *cli.Ctx, e *env) (any, error) {
	wait, waitGiven, err := c.Int("wait")
	if err != nil || (waitGiven && (wait < 0 || wait > upgradeMaxWait)) {
		return nil, cli.Inputf("wait: expected a number of seconds from 0 to %d", upgradeMaxWait)
	}
	if waitGiven && c.Bool("force") {
		return nil, cli.Inputf("give --wait or --force, not both")
	}
	if !version.ReleaseBuild() {
		return nil, fmt.Errorf("This is a development build (%s), not a release, so there is nothing to upgrade from. Nothing was changed.", version.Version)
	}
	binary, err := upgradeBinaryPath()
	if err != nil {
		return nil, err
	}
	if err := upgrade.CheckReplaceable(binary); err != nil {
		return nil, err
	}
	facts, arguments := gatherGatewayFacts(e, binary)
	state, err := upgrade.ClassifyGateway(facts)
	if err != nil {
		return nil, err
	}

	// Which release, before asking anything of the gateway.
	src := upgrade.DefaultSource()
	tag := c.String("version")
	if tag == "" {
		latest, err := src.Latest(context.Background(), "")
		if err != nil {
			return nil, err
		}
		tag = latest.Tag
	}
	if !upgrade.Compare("0.0.0", tag).Known {
		return nil, cli.Inputf("version: %q is not a release, such as v1.2.3", tag)
	}
	target := strings.TrimPrefix(tag, "v")
	data := map[string]any{"current": version.Version, "target": target}
	proceed, kind := upgrade.DecideVersion(version.Version, target, c.Bool("allow-downgrade"))
	switch {
	case kind == "same":
		c.Info("This is up to date: %s.", version.Version)
		data["changed"] = false
		return data, nil
	case kind == "downgrade":
		return nil, fmt.Errorf("%s is older than this version (%s). Nothing was changed. If you mean it: --allow-downgrade", tag, version.Version)
	case !proceed:
		return nil, fmt.Errorf("Cannot compare %s with %s. Nothing was changed.", tag, version.Version)
	}
	if !upgrade.Ready(upgrade.TrustedKeys()) {
		return nil, &upgrade.GateError{}
	}
	if free, err := install.FreeBytes(filepath.Dir(binary)); err == nil && free < upgradeNeedsBytes {
		return nil, fmt.Errorf("There is not enough room beside %s for the download. Nothing was changed.", binary)
	}

	// Busy, or not. --force skips asking.
	if state == upgrade.GatewayManaged && !c.Bool("force") {
		base, err := idleWaitBase(e)
		if err != nil {
			return nil, err
		}
		if err := waitGatewayIdle(c, e, base, time.Duration(wait)*time.Second, time.Sleep); err != nil {
			return nil, err
		}
	}

	c.Info("Downloading and verifying %s...", tag)
	cand, err := upgrade.Prepare(context.Background(), src, upgrade.PrepareOptions{Tag: tag, GOOS: goruntime.GOOS, GOARCH: goruntime.GOARCH,
		Dir: filepath.Dir(binary), Keys: upgrade.TrustedKeys(), Run: upgrade.RunVersion})
	if err != nil {
		return nil, err
	}
	defer cand.Remove()
	data["signedBy"] = cand.KeyID

	plan := "This replaces " + binary + " (" + version.Version + " to " + target + ") and keeps the old one for rollback."
	if state == upgrade.GatewayManaged {
		plan += " It stops the gateway, restarts it through the service manager, and runs smoke."
	}
	c.Info("%s", plan)
	if !c.Bool("yes") {
		if c.Global.JSON || c.Global.NonInteractive || !isTerminal(os.Stdin) {
			return nil, cli.Inputf("upgrade changes the installed binary. Re-run with --yes to confirm.")
		}
		fmt.Fprint(c.Stderr, "Continue? [y/N] ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" { // /dev/null looks like a terminal, and answers nothing
			return nil, cli.Inputf("upgrade changes the installed binary. Re-run with --yes to confirm.")
		}
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return nil, errors.New("Stopped. Nothing was changed.")
		}
	}

	ops := upgrade.Ops{}
	if state == upgrade.GatewayManaged {
		ops = managedOps(c, e, binary, arguments)
	}
	res, err := upgrade.Apply(cand, binary, ops)
	data["rolledBack"] = res.RolledBack
	e.audit.Upgrade("audit.upgrade-applied", version.Version, target, cand.SHA256, upgrade.ApplyOutcome(res, err))
	if err != nil {
		var smoke *upgrade.SmokeFailed
		if errors.As(err, &smoke) {
			c.Error("To go back: local-base upgrade rollback")
			c.Error("If that will not run: %s", rollbackLine(binary))
		}
		return nil, err
	}
	data["changed"] = true
	e.audit.WithResult(data)
	c.Info("Now running %s.", target)
	if state != upgrade.GatewayManaged {
		c.Info("No gateway was running here. To start one: local-base start")
	}
	c.Info("To go back: local-base upgrade rollback")
	c.Info("If that will not run: %s", rollbackLine(binary))
	return data, nil
}

// runCheckUpdates turns the daily check on or off. Off is the default.
func runCheckUpdates(c *cli.Ctx, e *env) (any, error) {
	var on bool
	switch c.Args[0] {
	case "on":
		on = true
	case "off":
	default:
		return nil, cli.Inputf("check-updates: expected on or off")
	}
	out, err := writeThroughGateway(e, "/_localbase/update", map[string]any{"action": "set-check", "enabled": on}, func() (map[string]any, error) {
		checker := upgrade.NewChecker(e.Store, upgrade.DefaultSource(), version.Version, version.ReleaseBuild(), nil)
		if err := checker.SetEnabled(context.Background(), on); err != nil {
			return nil, err
		}
		return map[string]any{"enabled": on}, nil
	})
	if err != nil {
		return nil, err
	}
	if on {
		c.Info("The daily check is on. Once a day, it asks GitHub for the latest release, and sends nothing about this computer. It installs nothing.")
	} else {
		c.Info("The daily check is off. local-base upgrade --check still works any time.")
	}
	return out, nil
}

func runUpgradeRollback(c *cli.Ctx, e *env) (any, error) {
	binary, err := upgradeBinaryPath()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(binary + upgrade.PreviousSuffix); err != nil {
		return nil, errors.New("There is no previous version to go back to. Nothing was changed.")
	}
	facts, arguments := gatherGatewayFacts(e, binary)
	state, err := upgrade.ClassifyGateway(facts)
	if err != nil {
		return nil, err
	}
	ops := upgrade.Ops{}
	if state == upgrade.GatewayManaged {
		ops = managedOps(c, e, binary, arguments)
	}
	// What it goes back to, for the record, if the file will say.
	to := "unknown"
	if v, err := upgrade.RunVersion(binary + upgrade.PreviousSuffix); err == nil {
		to = upgrade.VersionForAudit(v)
	}
	// The previous version doesn't keep the -wal owner record, so a crash under it would leave a -wal
	// the next upgrade refuses. Only once it's in place, and nothing else is removed.
	ops.OnPrevious = func() error { return store.ForgetWALOwner(e.Root.DB()) }
	if err := upgrade.Rollback(binary, ops); err != nil {
		e.audit.Upgrade("audit.upgrade-rolled-back", version.Version, to, "", "failed")
		return nil, err
	}
	e.audit.Upgrade("audit.upgrade-rolled-back", version.Version, to, "", "rolled-back")
	c.Info("Back on the previous version. To undo this: local-base upgrade rollback")
	return map[string]any{"rolledBack": true}, nil
}
