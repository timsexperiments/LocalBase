package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/install"
	"github.com/timsexperiments/LocalBase/go/internal/service"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/upgrade"
	"github.com/timsexperiments/LocalBase/go/internal/version"
)

const (
	upgradeMaxWait = 600 // seconds
	// upgradeNeedsBytes is room for an archive and its binary, beside the installed one.
	upgradeNeedsBytes = 512 << 20
)

// upgradeReadyWait is how long a restarted gateway gets to answer ready.
var upgradeReadyWait = 90 * time.Second

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

// releaseBuild: a release stamps its commit. A plain go build does not.
func releaseBuild() bool {
	return version.Commit != "" && !version.Get().Modified
}

func runUpgradeCheck(c *cli.Ctx) (any, error) {
	data := map[string]any{"current": version.Version, "release": releaseBuild()}
	if !releaseBuild() {
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
		c.Info("Release notes: %s", cli.PrintableASCII(latest.URL))
		c.Info("To install it: local-base upgrade")
	default:
		c.Info("This is up to date: %s (the latest release is %s).", version.Version, latest.Tag)
	}
	return data, nil
}

// gatewayState is what runs on this root, for an upgrade's purposes.
type gatewayState int

const (
	gatewayNone gatewayState = iota
	gatewayManaged
)

// classifyGateway decides from the facts, so each refusal can be tested on its own.
type gatewayFacts struct {
	lockState, lockDetail          string
	ownerManaged                   bool
	definitionInstalled, reachable bool
	serviceProgram, binary         string
}

func classifyGateway(f gatewayFacts) (gatewayState, error) {
	switch f.lockState {
	case "missing", "stale":
		return gatewayNone, nil
	case "active":
	default:
		detail := f.lockDetail
		if detail == "" {
			detail = "its lock could not be read"
		}
		return 0, errors.New("Cannot tell whether a gateway is running on this root: " + detail + ". Nothing was changed.")
	}
	if !f.ownerManaged {
		return 0, errors.New("A gateway is running on this root in the foreground. Stop it first: this cannot restart what it did not start. Nothing was changed.")
	}
	if !f.definitionInstalled || !f.reachable {
		return 0, errors.New("A gateway is running under the service manager, but the service manager cannot be reached, so it could not be restarted safely. Nothing was changed.")
	}
	if f.serviceProgram != f.binary {
		return 0, fmt.Errorf("The service on this root runs %s, not this binary (%s), so this could not restart it safely. Run that one's upgrade. Nothing was changed.", f.serviceProgram, f.binary)
	}
	return gatewayManaged, nil
}

func upgradeBinaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// checkInstalled refuses what cannot be replaced by this command.
func checkInstalled(binary string) error {
	if strings.Contains(binary, "/Cellar/") {
		return errors.New("This was installed by Homebrew. Update it with: brew upgrade local-base")
	}
	dir := filepath.Dir(binary)
	probe, err := os.CreateTemp(dir, ".local-base-upgrade-probe-")
	if err != nil {
		return fmt.Errorf("Cannot write in %s, so this cannot replace the binary there. Nothing was changed.", dir)
	}
	probe.Close()
	os.Remove(probe.Name())
	if upgrade.AssetArchive(goruntime.GOOS, goruntime.GOARCH) == "" {
		return fmt.Errorf("There are no releases for %s/%s.", goruntime.GOOS, goruntime.GOARCH)
	}
	return nil
}

// gatewayActivity asks the running gateway how busy it is, with a key that lasts this long.
func gatewayActivity(c *cli.Ctx, e *env, base string) (busy bool, detail string, err error) {
	ctx := context.Background()
	k, raw, err := e.Store.CreateAPIKeyExpiring(ctx, "local-base upgrade (temporary)", []string{"models:read"}, 1)
	if err != nil {
		return false, "", err
	}
	auditKeyChange(e, "create", k, "purpose", "upgrade")
	defer func() {
		if derr := e.Store.DeleteAPIKey(ctx, k.ID); derr == nil {
			auditKeyChange(e, "delete", k, "purpose", "upgrade")
		} else {
			c.Error("Could not delete the temporary key %s: %v. It can only list models and it expires in a day. To remove it now: local-base keys revoke %s", k.ID, derr, k.ID)
		}
	}()
	req, err := http.NewRequest(http.MethodGet, base+"/_localbase/host", nil)
	if err != nil {
		return false, "", err
	}
	req.Header.Set("Authorization", "Bearer "+raw)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return false, "", err
	}
	defer resp.Body.Close()
	var body struct {
		Host struct {
			Activity struct {
				Busy bool `json:"busy"`
				Runs struct {
					Active int `json:"active"`
				} `json:"runs"`
				LLM, STT, TTS, Image, Video struct {
					InFlight int `json:"inFlight"`
					Queued   int `json:"queued"`
				}
			} `json:"activity"`
		} `json:"host"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&body) != nil {
		return false, "", fmt.Errorf("the gateway answered HTTP %d", resp.StatusCode)
	}
	a := body.Host.Activity
	work := a.LLM.InFlight + a.STT.InFlight + a.TTS.InFlight + a.Image.InFlight + a.Video.InFlight
	queued := a.LLM.Queued + a.STT.Queued + a.TTS.Queued + a.Image.Queued + a.Video.Queued
	return a.Busy, fmt.Sprintf("%d in flight, %d queued, %d detached runs", work, queued, a.Runs.Active), nil
}

// waitIdle waits for a gateway's work to finish, and cuts nothing if it does not.
func waitIdle(ask func() (bool, string, error), limit time.Duration, sleep func(time.Duration)) error {
	deadline := time.Now().Add(limit)
	for {
		busy, detail, err := ask()
		if err != nil {
			return fmt.Errorf("Could not ask the gateway how busy it is: %w. Nothing was changed. --force goes ahead anyway.", err)
		}
		if !busy {
			return nil
		}
		if limit <= 0 {
			return fmt.Errorf("The gateway is busy: %s. Nothing was changed. --wait <seconds> waits for it to finish, and --force goes ahead, cutting that work.", detail)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("The gateway is still busy after waiting: %s. Nothing was changed, and nothing was cut. --force goes ahead, cutting that work.", detail)
		}
		sleep(time.Second)
	}
}

// backendPorts are where this root's backends listen, as configured. A flag can move the media
// ones, and those are read from how the service is started. Only a kind this root uses counts:
// another root's backend on a default port is not one this gateway started, and could not be
// told apart from a survivor.
func backendPorts(e *env, arguments []string) []int {
	// serve's flags as given: "--x value" or "--x=value".
	flag := func(name string) (int, bool) {
		for i, a := range arguments {
			if v, ok := strings.CutPrefix(a, "--"+name+"="); ok {
				n, err := strconv.Atoi(v)
				return n, err == nil
			}
			if a == "--"+name && i+1 < len(arguments) {
				n, err := strconv.Atoi(arguments[i+1])
				return n, err == nil
			}
		}
		return 0, false
	}
	// The same resolution as serve's: an explicit port, else --runtime-port-base's block, else the
	// configured or default port.
	base, _ := flag("runtime-port-base")
	defaults := map[string]int{"llm": e.Config.Port, "stt": e.Config.SttPort}
	for kind, p := range mediaDefaultPorts {
		defaults[kind] = p
	}
	out := make([]int, 0, len(runtimePortOrder))
	for _, kind := range runtimePortOrder {
		p, explicit := flag(kind + "-port")
		if !explicit {
			p = runtimePort(base, kind, defaults[kind])
		}
		if p > 0 && (len(e.Config.Selected[kind]) > 0 || e.Config.Active[kind] != "") {
			out = append(out, p)
		}
	}
	return out
}

func portListening(port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// managedOps stop and restart through the service manager. Stopping is confirmed, because the
// sweep of §29 cannot cover this: a backend from before that version has no record for it.
func managedOps(c *cli.Ctx, e *env, binary string, arguments []string) upgrade.Ops {
	return upgrade.Ops{
		Stop: func() error {
			c.Info("Stopping the gateway...")
			_, err := service.Stop(e.Root)
			auditService(e, "audit.service-stopped", err, "purpose", "upgrade", "disabled", err == nil)
			if err != nil {
				return err
			}
			for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(200 * time.Millisecond) {
				state, _, _ := e.Root.OwnerState()
				var survivors []string
				if state != "active" {
					for _, p := range backendPorts(e, arguments) {
						if portListening(p) {
							survivors = append(survivors, strconv.Itoa(p))
						}
					}
				}
				if state != "active" && len(survivors) == 0 {
					return nil
				}
				if time.Now().After(deadline) {
					if state == "active" {
						return errors.New("the gateway did not stop")
					}
					return errors.New("something still listens on backend port " + strings.Join(survivors, ", "))
				}
			}
		},
		Start: func() error {
			c.Info("Restarting through the service manager...")
			if err := runSelf(binary, e.Root.Dir, "restart"); err != nil {
				return err
			}
			// restart returns once the manager has taken it, which can be before the
			// gateway listens. Smoke would then fail on a release that is fine, and a
			// gateway that never comes up has to roll back.
			if err := service.WaitReady(e.Root, upgradeReadyWait); err != nil {
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

func gatherGatewayFacts(e *env, binary string) (gatewayFacts, []string) {
	state, owner, detail := e.Root.OwnerState()
	f := gatewayFacts{lockState: state, lockDetail: detail, binary: binary, ownerManaged: owner != nil && owner.ServiceID != ""}
	if state != "active" {
		return f, nil
	}
	if in, err := service.Inspect(e.Root, false); err == nil {
		f.definitionInstalled, f.reachable = in.Service.DefinitionInstalled, in.Service.ManagerAvailable
	}
	program, arguments := service.InstalledInvocation(e.Root.Dir)
	f.serviceProgram = program
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
	if !releaseBuild() {
		return nil, fmt.Errorf("This is a development build (%s), not a release, so there is nothing to upgrade from. Nothing was changed.", version.Version)
	}
	binary, err := upgradeBinaryPath()
	if err != nil {
		return nil, err
	}
	if err := checkInstalled(binary); err != nil {
		return nil, err
	}
	facts, arguments := gatherGatewayFacts(e, binary)
	state, err := classifyGateway(facts)
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
	if state == gatewayManaged && !c.Bool("force") {
		_, owner, _ := e.Root.OwnerState()
		base := strings.TrimSuffix(owner.HealthURL(), "/health")
		if err := waitIdle(func() (bool, string, error) { return gatewayActivity(c, e, base) }, time.Duration(wait)*time.Second, time.Sleep); err != nil {
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
	if state == gatewayManaged {
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
	if state == gatewayManaged {
		ops = managedOps(c, e, binary, arguments)
	}
	res, err := upgrade.Apply(cand, binary, ops)
	data["rolledBack"] = res.RolledBack
	auditUpgrade(e, "audit.upgrade-applied", version.Version, target, cand.SHA256, upgradeOutcome(res, err))
	if err != nil {
		var smoke *upgrade.SmokeFailed
		if errors.As(err, &smoke) {
			c.Error("To go back: local-base upgrade rollback")
			c.Error("If that will not run: %s", rollbackLine(binary))
		}
		return nil, err
	}
	data["changed"] = true
	e.withAuditResult(data)
	c.Info("Now running %s.", target)
	if state != gatewayManaged {
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
		checker := upgrade.NewChecker(e.Store, upgrade.DefaultSource(), version.Version, releaseBuild(), nil)
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
	state, err := classifyGateway(facts)
	if err != nil {
		return nil, err
	}
	ops := upgrade.Ops{}
	if state == gatewayManaged {
		ops = managedOps(c, e, binary, arguments)
	}
	// What it goes back to, for the record, if the file will say.
	to := "unknown"
	if v, err := upgrade.RunVersion(binary + upgrade.PreviousSuffix); err == nil {
		to = versionForAudit(v)
	}
	// The previous version doesn't keep the -wal owner record, so a crash under it would leave a -wal
	// the next upgrade refuses. Only once it's in place, and nothing else is removed.
	ops.OnPrevious = func() error { return store.ForgetWALOwner(e.Root.DB()) }
	if err := upgrade.Rollback(binary, ops); err != nil {
		auditUpgrade(e, "audit.upgrade-rolled-back", version.Version, to, "", "failed")
		return nil, err
	}
	auditUpgrade(e, "audit.upgrade-rolled-back", version.Version, to, "", "rolled-back")
	c.Info("Back on the previous version. To undo this: local-base upgrade rollback")
	return map[string]any{"rolledBack": true}, nil
}
