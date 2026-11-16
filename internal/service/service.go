// Package service manages the LocalBase user service: a launchd agent on
// macOS or a systemd user unit on Linux, with the same label, definition
// text, manifest and status rules as the TS implementation, so either binary
// can manage a service the other installed.
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/root"
)

const managerTimeout = 10 * time.Second

// Metadata names the service for a root.
type Metadata struct {
	Platform       string // darwin | linux
	Manager        string // launchd | systemd-user
	ServiceID      string
	UnitName       string
	DefinitionPath string
}

// Error is a service-manager failure (operational, exit 1).
type Error struct{ Message string }

func (e *Error) Error() string { return e.Message }

func errorf(format string, a ...any) error { return &Error{fmt.Sprintf(format, a...)} }

// MetadataFor returns the service identity of a canonical root.
func MetadataFor(rootDir string) (Metadata, error) {
	platform := goruntime.GOOS
	if platform != "darwin" && platform != "linux" {
		return Metadata{}, errorf("User service management is supported on macOS (launchd) and Linux (systemd --user); use \"local-base serve\" in the foreground on %s.", platform)
	}
	hash := root.Hash(rootDir)
	m := Metadata{Platform: platform, ServiceID: "com.localbase.gateway." + hash,
		UnitName: "local-base-gateway-" + hash + ".service"}
	home := os.Getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if platform == "darwin" {
		m.Manager = "launchd"
		m.DefinitionPath = filepath.Join(home, "Library", "LaunchAgents", m.ServiceID+".plist")
	} else {
		m.Manager = "systemd-user"
		cfgHome := os.Getenv("XDG_CONFIG_HOME")
		if cfgHome == "" {
			cfgHome = filepath.Join(home, ".config")
		}
		m.DefinitionPath = filepath.Join(cfgHome, "systemd", "user", m.UnitName)
	}
	return m, nil
}

// Invocation is the command line the service runs.
type Invocation struct {
	Program      string   `json:"program"`
	Arguments    []string `json:"arguments"`
	ServiceToken string   `json:"serviceToken,omitempty"`
}

// Manifest is `<root>/service/manifest.json`.
type Manifest struct {
	Version               int        `json:"version"`
	Root                  string     `json:"root"`
	RootHash              string     `json:"rootHash"`
	Manager               string     `json:"manager"`
	ServiceID             string     `json:"serviceId"`
	DefinitionPath        string     `json:"definitionPath"`
	DefinitionFingerprint string     `json:"definitionFingerprint"`
	ServiceToken          string     `json:"serviceToken"`
	Invocation            Invocation `json:"invocation"`
}

// InstalledInvocation is what the service on this root runs, or "" with no valid manifest.
func InstalledInvocation(rootDir string) (program string, arguments []string) {
	state, m := readManifest(rootDir)
	if state != "valid" || m == nil {
		return "", nil
	}
	return m.Invocation.Program, m.Invocation.Arguments
}

func manifestPath(rootDir string) string { return filepath.Join(rootDir, "service", "manifest.json") }

// readManifest returns missing, valid (with the manifest) or invalid.
func readManifest(rootDir string) (string, *Manifest) {
	b, err := os.ReadFile(manifestPath(rootDir))
	if errors.Is(err, os.ErrNotExist) {
		return "missing", nil
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err != nil || dec.Decode(&m) != nil || m.Version != 1 || m.ServiceToken == "" || m.Invocation.Program == "" {
		return "invalid", nil
	}
	return "valid", &m
}

// --- manager commands ----------------------------------------------------------------

type result struct {
	code           int
	stdout, stderr string
}

// Manager runs the user service manager's commands (launchctl, systemctl) for a root's service:
// starting, stopping, removing and inspecting it. Every command one operation runs goes through the
// Manager it was asked of, so a test's Manager sees them all.
type Manager struct {
	// runner runs one manager command (tests: a fake that records what it was asked); nil runs it.
	runner func(args ...string) (result, error)
}

// NewManager is the Manager that runs the system's service manager.
func NewManager() Manager { return Manager{} }

// runCommand runs a manager command.
func runCommand(args ...string) (result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), managerTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ctx.Err() != nil {
		return result{}, errorf("%s timed out after %dms. Use foreground \"local-base serve\" until the user service manager is available.",
			filepath.Base(args[0]), managerTimeout.Milliseconds())
	}
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		return result{}, errorf("Could not start %s. Use foreground \"local-base serve\" if the user service manager is unavailable.", args[0])
	}
	return result{code, out.String(), errb.String()}, nil
}

func executable(m Metadata) string {
	if m.Manager == "launchd" {
		return "/bin/launchctl"
	}
	return "/usr/bin/systemctl"
}

func (g Manager) run(m Metadata, args ...string) (result, error) {
	argv := append([]string{executable(m)}, args...)
	if g.runner != nil {
		return g.runner(argv...)
	}
	return runCommand(argv...)
}

func (g Manager) must(m Metadata, args ...string) error {
	r, err := g.run(m, args...)
	if err != nil {
		return err
	}
	if r.code != 0 {
		detail := strings.TrimSpace(r.stderr)
		if detail == "" {
			detail = strings.TrimSpace(r.stdout)
		}
		if detail == "" {
			detail = "exit code " + strconv.Itoa(r.code)
		}
		return errorf("%s could not manage the LocalBase service: %s.", filepath.Base(executable(m)), detail)
	}
	return nil
}

// observation is what the service manager reports.
type observation struct {
	available, loaded, active, stopping, failed bool
	enabled                                     *bool
	state                                       string
	pid, restarts                               int
	hasRestarts                                 bool
}

func (g Manager) inspect(m Metadata) (observation, error) {
	if m.Manager == "launchd" {
		if r, err := g.run(m, "print", domain()); err != nil || r.code != 0 {
			return observation{}, errorf("The launchd GUI user domain is unavailable. Sign in to a macOS user session or use foreground \"local-base serve\".")
		}
		r, err := g.run(m, "print", target(m))
		if err != nil {
			return observation{}, err
		}
		o := observation{available: true}
		if d, err := g.run(m, "print-disabled", domain()); err == nil && d.code == 0 {
			o.enabled = parseLaunchdEnabled(d.stdout, m.ServiceID)
		}
		if r.code != 0 {
			if r.code != 113 {
				return observation{}, errorf("launchctl could not manage the LocalBase service: %s.", strings.TrimSpace(r.stderr+r.stdout))
			}
			o.state = "unloaded"
			return o, nil
		}
		state, pid, lastExit, err := parseLaunchctl(r.stdout)
		if err != nil {
			return observation{}, err
		}
		running := state == "running" && pid > 0
		o.loaded, o.state, o.pid = true, state, pid
		o.active = running || state == "spawn scheduled"
		o.failed = lastExit != nil && *lastExit != 0 && !running
		return o, nil
	}
	r, err := g.run(m, "--user", "show", m.UnitName,
		"--property=LoadState,ActiveState,SubState,MainPID,ExecMainCode,ExecMainStatus,NRestarts", "--no-pager")
	if err != nil || r.code != 0 {
		return observation{}, errorf("The systemd user manager is unavailable. Ensure the user bus is running or use foreground \"local-base serve\".")
	}
	rec := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(r.stdout), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			rec[k] = v
		}
	}
	o := observation{available: true, state: rec["LoadState"] + "/" + rec["ActiveState"] + "/" + rec["SubState"]}
	o.loaded = rec["LoadState"] == "loaded"
	o.active = rec["ActiveState"] == "active" || rec["ActiveState"] == "activating" || rec["ActiveState"] == "reloading"
	o.stopping = rec["ActiveState"] == "deactivating"
	o.failed = rec["ActiveState"] == "failed" || rec["LoadState"] == "error"
	o.pid, _ = strconv.Atoi(rec["MainPID"])
	if n, err := strconv.Atoi(rec["NRestarts"]); err == nil {
		o.restarts, o.hasRestarts = n, true
	}
	if e, err := g.run(m, "--user", "is-enabled", m.UnitName); err == nil {
		v := strings.TrimSpace(e.stdout)
		t, f := true, false
		switch {
		case e.code == 0 && (v == "enabled" || v == "enabled-runtime" || v == "linked" || v == "linked-runtime"):
			o.enabled = &t
		case v == "disabled" || v == "masked" || v == "static" || v == "indirect" || v == "generated":
			o.enabled = &f
		}
	}
	return o, nil
}

func (o observation) stopped(m Metadata) bool {
	if m.Manager == "launchd" {
		return !o.loaded
	}
	return !o.active && !o.stopping
}
