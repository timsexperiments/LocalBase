// Package service manages the LocalBase user service: a launchd agent on
// macOS or a systemd user unit on Linux, with the same label, definition
// text, manifest and status rules as the TS implementation, so either binary
// can manage a service the other installed.
package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
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

var otelNames = []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
	"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS", "OTEL_EXPORTER_OTLP_LOGS_HEADERS",
	"OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL",
	"OTEL_TRACES_SAMPLER", "OTEL_TRACES_SAMPLER_ARG"}

// otelEnvironment passes the caller's OTEL_* settings into the service.
func otelEnvironment() map[string]string {
	env := map[string]string{}
	for _, n := range otelNames {
		if v := os.Getenv(n); v != "" {
			env[n] = v
		}
	}
	return env
}

var xmlReplacer = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")

func xmlEscape(s string) string { return xmlReplacer.Replace(s) }

// RenderLaunchd renders the agent plist exactly as TS does.
func RenderLaunchd(m Metadata, inv Invocation, rootDir, token string, otel map[string]string) string {
	args := append([]string{inv.Program}, inv.Arguments...)
	stderr := filepath.Join(rootDir, "logs", "service-stderr.log")
	lines := []string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">`,
		`<plist version="1.0">`,
		"<dict>",
		"  <key>Label</key><string>" + xmlEscape(m.ServiceID) + "</string>",
		"  <key>Program</key><string>" + xmlEscape(inv.Program) + "</string>",
		"  <key>ProgramArguments</key>",
		"  <array>",
	}
	for _, a := range args {
		lines = append(lines, "    <string>"+xmlEscape(a)+"</string>")
	}
	lines = append(lines, "  </array>",
		"  <key>WorkingDirectory</key><string>"+xmlEscape(rootDir)+"</string>",
		"  <key>RunAtLoad</key><true/>",
		"  <key>KeepAlive</key><true/>",
		"  <key>ExitTimeOut</key><integer>15</integer>",
		"  <key>Umask</key><integer>63</integer>",
		"  <key>ProcessType</key><string>Interactive</string>",
		"  <key>EnvironmentVariables</key>",
		"  <dict>",
		"    <key>LOCALBASE_SERVICE_ID</key><string>"+xmlEscape(m.ServiceID)+"</string>",
		"    <key>LOCALBASE_SERVICE_TOKEN</key><string>"+xmlEscape(token)+"</string>")
	for _, k := range sortedKeys(otel) {
		lines = append(lines, "    <key>"+xmlEscape(k)+"</key><string>"+xmlEscape(otel[k])+"</string>")
	}
	lines = append(lines, "  </dict>",
		"  <key>StandardOutPath</key><string>/dev/null</string>",
		"  <key>StandardErrorPath</key><string>"+xmlEscape(stderr)+"</string>",
		"</dict>", "</plist>", "")
	return strings.Join(lines, "\n")
}

func quoteSystemd(v string, expandDollar bool) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range v {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '"':
			b.WriteString(`\"`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '%':
			b.WriteString("%%")
		case r == '$' && expandDollar:
			b.WriteString("$$")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func escapeSystemdPath(v string) string {
	var b strings.Builder
	for _, r := range v {
		switch r {
		case '%':
			b.WriteString("%%")
		case ' ':
			b.WriteString(`\x20`)
		case '\t':
			b.WriteString(`\x09`)
		case '"':
			b.WriteString(`\x22`)
		case '\\':
			b.WriteString(`\x5c`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// RenderSystemd renders the user unit exactly as TS does.
func RenderSystemd(m Metadata, inv Invocation, rootDir, token string, otel map[string]string) string {
	exec := []string{}
	for _, a := range append([]string{inv.Program}, inv.Arguments...) {
		exec = append(exec, quoteSystemd(a, true))
	}
	lines := []string{"[Unit]", "Description=LocalBase gateway", "After=network.target", "", "[Service]", "Type=exec",
		"ExecStart=" + strings.Join(exec, " "), "WorkingDirectory=" + escapeSystemdPath(rootDir), "Restart=on-failure",
		"RestartSec=2s", "KillMode=mixed", "KillSignal=SIGTERM", "TimeoutStopSec=15s", "UMask=0077",
		"Environment=" + quoteSystemd("LOCALBASE_SERVICE_ID="+m.ServiceID, false),
		"Environment=" + quoteSystemd("LOCALBASE_SERVICE_TOKEN="+token, false)}
	for _, k := range sortedKeys(otel) {
		lines = append(lines, "Environment="+quoteSystemd(k+"="+otel[k], false))
	}
	lines = append(lines, "StandardOutput=journal", "StandardError=journal", "", "[Install]", "WantedBy=default.target", "")
	return strings.Join(lines, "\n")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func fingerprint(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// ResolveInvocation is this executable serving the root.
func ResolveInvocation(rootDir string, installMissing bool) (Invocation, error) {
	exe, err := os.Executable()
	if err != nil {
		return Invocation{}, err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return Invocation{}, err
	}
	args := []string{"--root", rootDir, "serve"}
	if installMissing {
		args = append(args, "--install-missing")
	}
	return Invocation{Program: exe, Arguments: args}, nil
}

// Definition is a rendered service definition and the manifest recording it.
type Definition struct {
	Metadata
	Contents string
	Manifest Manifest
}

func newDefinition(rootDir string, inv Invocation, token string) (Definition, error) {
	m, err := MetadataFor(rootDir)
	if err != nil {
		return Definition{}, err
	}
	otel := otelEnvironment()
	contents := RenderSystemd(m, inv, rootDir, token, otel)
	if m.Manager == "launchd" {
		contents = RenderLaunchd(m, inv, rootDir, token, otel)
	}
	bound := inv
	bound.ServiceToken = token
	return Definition{Metadata: m, Contents: contents, Manifest: Manifest{Version: 1, Root: rootDir, RootHash: root.Hash(rootDir),
		Manager: m.Manager, ServiceID: m.ServiceID, DefinitionPath: m.DefinitionPath,
		DefinitionFingerprint: fingerprint([]byte(contents)), ServiceToken: token, Invocation: bound}}, nil
}

func atomicWrite(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + "." + store.UUID() + ".tmp"
	defer os.Remove(tmp)
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (d Definition) write(rootDir string) error {
	if d.Manager == "launchd" {
		// launchd opens the stderr path itself; create it private first.
		if err := os.MkdirAll(filepath.Join(rootDir, "logs"), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(filepath.Join(rootDir, "logs", "service-stderr.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		f.Close()
	}
	if err := atomicWrite(d.DefinitionPath, []byte(d.Contents)); err != nil {
		return err
	}
	b, _ := json.Marshal(d.Manifest)
	return atomicWrite(manifestPath(rootDir), b)
}

// --- manager commands ----------------------------------------------------------------

type result struct {
	code           int
	stdout, stderr string
}

// Runner runs a manager command (tests replace it).
var Runner = func(args ...string) (result, error) {
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

func run(m Metadata, args ...string) (result, error) {
	return Runner(append([]string{executable(m)}, args...)...)
}

func must(m Metadata, args ...string) error {
	r, err := run(m, args...)
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

func domain() string           { return "gui/" + strconv.Itoa(os.Getuid()) }
func target(m Metadata) string { return domain() + "/" + m.ServiceID }

// observation is what the service manager reports.
type observation struct {
	available, loaded, active, stopping, failed bool
	enabled                                     *bool
	state                                       string
	pid, restarts                               int
	hasRestarts                                 bool
}

var launchdField = regexp.MustCompile(`^([^=]+?)\s*=\s*(.*?)\s*$`)

// parseLaunchctl reads the top-level state, pid and last exit code.
func parseLaunchctl(out string) (state string, pid int, lastExit *int, err error) {
	depth := 0
	fields := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if depth == 1 {
			if m := launchdField.FindStringSubmatch(t); m != nil && !strings.HasSuffix(m[2], "{") {
				k := strings.TrimSpace(m[1])
				if k == "state" || k == "pid" || k == "last exit code" {
					if _, dup := fields[k]; dup {
						return "", 0, nil, errorf("launchctl returned duplicate top-level %s.", k)
					}
					fields[k] = m[2]
				}
			}
		}
		depth += strings.Count(line, "{") - strings.Count(line, "}")
		if depth < 0 {
			return "", 0, nil, errorf("launchctl returned malformed service state.")
		}
	}
	if depth != 0 || fields["state"] == "" {
		return "", 0, nil, errorf("launchctl returned malformed service state.")
	}
	pid, _ = strconv.Atoi(fields["pid"])
	if v, ok := fields["last exit code"]; ok && v != "(never exited)" {
		if n, err := strconv.Atoi(v); err == nil {
			lastExit = &n
		}
	}
	return fields["state"], pid, lastExit, nil
}

func parseLaunchdEnabled(out, id string) *bool {
	for _, line := range strings.Split(out, "\n") {
		label, value, ok := strings.Cut(line, "=>")
		if !ok || strings.Trim(strings.TrimSpace(label), `"`) != id {
			continue
		}
		switch strings.TrimSuffix(strings.TrimSpace(value), ";") {
		case "enabled":
			t := true
			return &t
		case "disabled":
			f := false
			return &f
		}
	}
	return nil
}

func inspect(m Metadata) (observation, error) {
	if m.Manager == "launchd" {
		if r, err := run(m, "print", domain()); err != nil || r.code != 0 {
			return observation{}, errorf("The launchd GUI user domain is unavailable. Sign in to a macOS user session or use foreground \"local-base serve\".")
		}
		r, err := run(m, "print", target(m))
		if err != nil {
			return observation{}, err
		}
		o := observation{available: true}
		if d, err := run(m, "print-disabled", domain()); err == nil && d.code == 0 {
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
	r, err := run(m, "--user", "show", m.UnitName,
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
	if e, err := run(m, "--user", "is-enabled", m.UnitName); err == nil {
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

// --- status ------------------------------------------------------------------------------

// Status mirrors TS ServiceStatus.
type Status struct {
	Manager             string  `json:"manager"`
	ServiceID           string  `json:"serviceId"`
	Root                string  `json:"root"`
	DefinitionPath      string  `json:"definitionPath"`
	DefinitionInstalled bool    `json:"definitionInstalled"`
	Enabled             *bool   `json:"enabled"`
	State               string  `json:"state"`
	ManagerAvailable    bool    `json:"managerAvailable"`
	ManagerState        *string `json:"managerState"`
	PID                 *int    `json:"pid"`
	UptimeSeconds       *int    `json:"uptimeSeconds"`
	RestartCount        *int    `json:"restartCount"`
	BaseURL             string  `json:"baseUrl,omitempty"`
}

// Gateway is the gateway readiness part of an inspection.
type Gateway struct {
	State  string          `json:"state"` // ready | not_ready
	URL    string          `json:"url,omitempty"`
	Detail string          `json:"detail,omitempty"`
	Health json.RawMessage `json:"health,omitempty"`
}

// Inspection is service status plus gateway readiness.
type Inspection struct {
	Service Status  `json:"service"`
	Gateway Gateway `json:"gateway"`
}

// ownerState verifies a live owner by its identity endpoint (#PID reuse).
func ownerState(r *root.Root) (state string, o *root.Owner, detail string) {
	state, o, detail = r.OwnerState()
	if state != "active" {
		return
	}
	url := strings.TrimSuffix(o.HealthURL(), "/health") + "/_localbase/instance"
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("X-Localbase-Instance-Token", o.InstanceToken)
	client := &http.Client{Timeout: 750 * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		return "unknown", o, "gateway process is present but did not prove its instance identity"
	}
	defer resp.Body.Close()
	var id struct{ InstanceID, RootHash string }
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&id) != nil || id.InstanceID != o.InstanceID || id.RootHash != o.RootHash {
		return "unknown", o, "gateway process is present but did not prove its instance identity"
	}
	return "active", o, ""
}

func readHealth(o *root.Owner) json.RawMessage {
	client := &http.Client{Timeout: 750 * time.Millisecond}
	resp, err := client.Get(o.HealthURL())
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var raw json.RawMessage
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&raw) != nil {
		return nil
	}
	return raw
}

// Inspect reports the service and gateway state of a root.
func Inspect(r *root.Root, acceptedStart bool) (Inspection, error) {
	m, err := MetadataFor(r.Dir)
	if err != nil {
		return Inspection{}, err
	}
	def, defErr := os.ReadFile(m.DefinitionPath)
	installed := defErr == nil
	obs, err := inspect(m)
	if err != nil {
		obs = observation{state: "unavailable"}
	}
	ownerSt, owner, detail := ownerState(r)
	manifestSt, manifest := readManifest(r.Dir)

	var recorded *root.Owner
	if ownerSt == "active" || ownerSt == "stale" || (ownerSt == "unknown" && owner != nil) {
		recorded = owner
	}
	tokenMatches := recorded != nil && manifest != nil && recorded.ServiceID == m.ServiceID && recorded.ServiceToken == manifest.ServiceToken
	pidMatches := m.Manager != "systemd-user" || (recorded != nil && obs.pid == recorded.PID)
	owned := tokenMatches && pidMatches
	foreground := recorded != nil && recorded.ServiceID == ""
	identityMismatch := recorded != nil && recorded.ServiceID == m.ServiceID && !owned
	manifestIssue := manifestSt == "invalid" || (manifest != nil && (manifest.Root != r.Dir || manifest.RootHash != root.Hash(r.Dir) ||
		manifest.Manager != m.Manager || manifest.ServiceID != m.ServiceID || manifest.DefinitionPath != m.DefinitionPath ||
		(installed && fingerprint(def) != manifest.DefinitionFingerprint))) ||
		(manifestSt == "missing" && (installed || obs.loaded || owned))
	active := ownerSt == "active"

	var state string
	switch {
	case ownerSt == "invalid", identityMismatch:
		state = "unknown"
	case foreground:
		state = map[bool]string{true: "foreground", false: "unknown"}[active]
	case !obs.available, recorded != nil && !owned, manifestIssue:
		state = "unknown"
	case obs.failed, ownerSt == "stale":
		state = "failed"
	case obs.stopping:
		state = "stopping"
	case active && owned && obs.active:
		state = "running"
	case obs.active && (ownerSt == "missing" || (ownerSt == "unknown" && (owned || owner == nil))):
		state = "starting"
	case active && owned && !obs.active:
		state = "unknown"
	case acceptedStart && m.Manager == "launchd" && obs.loaded && !obs.failed &&
		contains([]string{"stopped", "waiting", "spawn scheduled", "xpcproxy", "running"}, obs.state) &&
		(ownerSt == "missing" || (ownerSt == "unknown" && owner == nil)):
		state = "starting"
	case !installed && manifestSt == "missing" && !obs.loaded && ownerSt == "missing":
		state = "not_installed"
	default:
		state = "stopped"
	}
	st := Status{Manager: m.Manager, ServiceID: m.ServiceID, Root: r.Dir, DefinitionPath: m.DefinitionPath,
		DefinitionInstalled: installed, Enabled: obs.enabled, State: state, ManagerAvailable: obs.available}
	if obs.available {
		ms := obs.state
		switch {
		case ownerSt == "invalid":
			ms += "/owner-invalid"
		case identityMismatch:
			ms += "/managed-identity-mismatch"
		case ownerSt == "unknown":
			ms += "/owner-unverified"
		case manifestIssue:
			ms += "/manifest-mismatch"
		}
		st.ManagerState = &ms
	}
	if active {
		pid := owner.PID
		st.PID = &pid
		if t, err := time.Parse(time.RFC3339Nano, owner.StartedAt); err == nil {
			up := max(0, int(time.Since(t).Seconds()))
			st.UptimeSeconds = &up
		}
	}
	if m.Manager == "systemd-user" && obs.hasRestarts {
		n := obs.restarts
		st.RestartCount = &n
	}
	if recorded != nil {
		st.BaseURL = strings.TrimSuffix(recorded.HealthURL(), "/health")
	}
	gw := Gateway{State: "not_ready"}
	switch {
	case active:
		gw.URL = owner.HealthURL()
		if h := readHealth(owner); h != nil && healthOK(h) {
			gw.State, gw.Health = "ready", h
		} else {
			gw.Detail, gw.Health = "gateway health endpoint is unavailable", h
		}
	case ownerSt == "unknown" && owner != nil:
		gw.URL, gw.Detail = owner.HealthURL(), detail
	case ownerSt == "stale":
		gw.URL, gw.Detail = owner.HealthURL(), "gateway owner process is no longer running"
	case ownerSt == "missing":
		gw.Detail = "no active LocalBase instance"
	default:
		gw.Detail = detail
	}
	return Inspection{Service: st, Gateway: gw}, nil
}

func healthOK(raw json.RawMessage) bool {
	var h struct{ Status string }
	return json.Unmarshal(raw, &h) == nil && h.Status == "ok"
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// --- control ---------------------------------------------------------------------------

func assertControllable(r *root.Root, m Metadata, token, op string) error {
	state, o, detail := ownerState(r)
	switch {
	case state == "invalid", state == "unknown" && o == nil:
		return errorf("Cannot %s LocalBase for %s: %s.", op, r.Dir, detail)
	case (state == "active" || state == "unknown") && o.ServiceID == m.ServiceID && o.ServiceToken != token:
		return errorf("Cannot %s LocalBase for %s: the managed gateway identity does not match this service definition.", op, r.Dir)
	case (state == "active" || state == "unknown") && o.ServiceID != m.ServiceID:
		who := "a foreground gateway"
		if o.ServiceID != "" {
			who = "another managed gateway (" + o.ServiceID + ")"
		}
		return errorf("Cannot %s LocalBase for %s: %s owns the root. Stop it explicitly before managing the user service.", op, r.Dir, who)
	}
	return nil
}

func enable(m Metadata) error {
	if m.Manager == "launchd" {
		return must(m, "enable", target(m))
	}
	return must(m, "--user", "enable", m.UnitName)
}

func disable(m Metadata) error {
	if m.Manager == "launchd" {
		return must(m, "disable", target(m))
	}
	res, err := run(m, "--user", "disable", m.UnitName)
	if err != nil || res.code == 0 {
		return err
	}
	low := strings.ToLower(res.stdout + "\n" + res.stderr)
	if strings.Contains(low, "does not exist") || strings.Contains(low, "not found") {
		return nil
	}
	return must(m, "--user", "disable", m.UnitName)
}

func startManager(d Definition) error {
	if d.Manager == "launchd" {
		if err := enable(d.Metadata); err != nil {
			return err
		}
		return must(d.Metadata, "bootstrap", domain(), d.DefinitionPath)
	}
	if err := must(d.Metadata, "--user", "daemon-reload"); err != nil {
		return err
	}
	if err := enable(d.Metadata); err != nil {
		return err
	}
	return must(d.Metadata, "--user", "start", d.UnitName)
}

func stopManager(m Metadata, alsoDisable bool) error {
	before, err := inspect(m)
	if err != nil {
		return err
	}
	if m.Manager == "launchd" {
		if before.loaded {
			if err := must(m, "bootout", target(m)); err != nil {
				return err
			}
		}
	} else if before.loaded || before.active {
		if err := must(m, "--user", "stop", m.UnitName); err != nil {
			return err
		}
	}
	if alsoDisable {
		if err := disable(m); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(managerTimeout)
	for time.Now().Before(deadline) {
		if o, err := inspect(m); err == nil && o.stopped(m) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errorf("%s did not stop %s within %dms.", m.Manager, m.ServiceID, managerTimeout.Milliseconds())
}

func waitOwnerGone(r *root.Root) error {
	deadline := time.Now().Add(managerTimeout)
	for time.Now().Before(deadline) {
		state, _, detail := r.OwnerState()
		switch state {
		case "missing":
			return nil
		case "stale":
			if r.ClearStale() == nil {
				return nil
			}
		case "invalid":
			return errorf("Gateway ownership became invalid while stopping %s: %s.", r.Dir, detail)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errorf("LocalBase gateway for %s did not release ownership within %dms. No process was signaled because its identity could not be proven.",
		r.Dir, managerTimeout.Milliseconds())
}

func stopManaged(r *root.Root, m Metadata, alsoDisable bool) error {
	token := ""
	if st, man := readManifest(r.Dir); st == "valid" && man.Root == r.Dir && man.ServiceID == m.ServiceID {
		token = man.ServiceToken
	}
	if err := assertControllable(r, m, token, "stop"); err != nil {
		return err
	}
	r.ClearStale()
	state, o, _ := ownerState(r)
	obs, err := inspect(m)
	if err != nil {
		return err
	}
	if (state == "active" || state == "unknown") && o != nil && o.ServiceID == m.ServiceID && !obs.active && !obs.stopping {
		return errorf("Cannot stop LocalBase for %s: the recorded managed gateway is not controlled by the current %s instance. No process was signaled.", r.Dir, m.Manager)
	}
	if err := stopManager(m, alsoDisable); err != nil {
		return err
	}
	return waitOwnerGone(r)
}

// Start installs and starts the service (restart replaces a running one
// with a fresh token).
// Start installs and starts the service, holding the root operation lease and
// handing it to the gateway it launches.
func Start(r *root.Root, restart, installMissing bool) (Inspection, error) {
	lease, err := root.AcquireOperation(r.Dir, "service-start")
	if err != nil {
		return Inspection{}, err
	}
	defer lease.Release()
	return start(r, lease, restart, installMissing)
}

// RestartWithin restarts the service for a caller holding the operation
// lease (TS restartServiceWithinOperation); the lease is handed to the new
// gateway.
func RestartWithin(r *root.Root, lease *root.Lease) (Inspection, error) {
	return start(r, lease, true, false)
}

func start(r *root.Root, lease *root.Lease, restart, installMissing bool) (Inspection, error) {
	st, prev := readManifest(r.Dir)
	current := ""
	if st == "valid" && prev.Root == r.Dir {
		current = prev.ServiceToken
	}
	token := current
	if restart || token == "" {
		token = store.UUID()
	}
	inv, err := ResolveInvocation(r.Dir, installMissing)
	if err != nil {
		return Inspection{}, err
	}
	d, err := newDefinition(r.Dir, inv, token)
	if err != nil {
		return Inspection{}, err
	}
	check := current
	if check == "" {
		check = token
	}
	op := "start"
	if restart {
		op = "restart"
	}
	if err := assertControllable(r, d.Metadata, check, op); err != nil {
		return Inspection{}, err
	}
	r.ClearStale()
	state, o, _ := ownerState(r)
	obs, err := inspect(d.Metadata)
	if err != nil {
		return Inspection{}, err
	}
	installedDef, _ := os.ReadFile(d.DefinitionPath)
	unchanged := st == "valid" && prev.DefinitionFingerprint == d.Manifest.DefinitionFingerprint &&
		prev.ServiceID == d.ServiceID && prev.Root == r.Dir && fingerprint(installedDef) == d.Manifest.DefinitionFingerprint
	managedOwner := o != nil && o.ServiceID == d.ServiceID && (state == "active" || state == "unknown")
	if !restart && unchanged && obs.active && (managedOwner || state == "missing") {
		if err := enable(d.Metadata); err != nil {
			return Inspection{}, err
		}
		return Inspect(r, false)
	}
	if obs.loaded || obs.active || obs.stopping || managedOwner {
		if err := stopManaged(r, d.Metadata, true); err != nil {
			return Inspection{}, err
		}
	}
	if err := d.write(r.Dir); err != nil {
		return Inspection{}, err
	}
	if err := startManager(d); err != nil {
		return Inspection{}, err
	}
	if err := lease.Handoff(token); err != nil {
		return Inspection{}, err
	}
	return Inspect(r, true)
}

// Stop stops and disables the service under the root operation lease.
func Stop(r *root.Root) (in Inspection, err error) {
	err = root.WithOperation(r.Dir, "stop", func() error {
		in, err = StopWithin(r)
		return err
	})
	return in, err
}

// StopWithin is Stop for a caller that holds the operation lease.
func StopWithin(r *root.Root) (Inspection, error) {
	m, err := MetadataFor(r.Dir)
	if err != nil {
		return Inspection{}, err
	}
	if err := stopManaged(r, m, true); err != nil {
		return Inspection{}, err
	}
	return Inspect(r, false)
}

// HasEvidence reports whether anything of a service exists for the root.
func HasEvidence(r *root.Root) bool {
	m, err := MetadataFor(r.Dir)
	if err != nil {
		return false
	}
	_, defErr := os.Stat(m.DefinitionPath)
	_, manErr := os.Stat(manifestPath(r.Dir))
	state, _, _ := r.OwnerState()
	return defErr == nil || manErr == nil || state != "missing"
}

// Remove stops the service and deletes its definition and manifest. The
// caller holds the operation lease.
func Remove(r *root.Root) (Inspection, error) {
	m, err := MetadataFor(r.Dir)
	if err != nil {
		return Inspection{}, err
	}
	if err := stopManaged(r, m, true); err != nil {
		return Inspection{}, err
	}
	os.Remove(m.DefinitionPath)
	os.Remove(manifestPath(r.Dir))
	if m.Manager == "systemd-user" {
		if err := must(m, "--user", "daemon-reload"); err != nil {
			return Inspection{}, err
		}
	}
	return Inspect(r, false)
}

// WaitReady waits until the gateway answers /health/ready with ready and
// no restart is pending.
func WaitReady(r *root.Root, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 750 * time.Millisecond}
	for {
		if in, err := Inspect(r, false); err == nil && in.Gateway.State == "ready" {
			resp, err := client.Get(strings.TrimSuffix(in.Gateway.URL, "/health") + "/health/ready")
			if err == nil {
				var body struct{ Status string }
				json.NewDecoder(resp.Body).Decode(&body)
				resp.Body.Close()
				if resp.StatusCode == 200 && body.Status == "ready" && !restartPending(r) {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return errors.New("Timed out waiting for LocalBase gateway readiness.")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// restartPending reports a recorded pending restart (read-only).
func restartPending(r *root.Root) bool {
	st, err := store.OpenReadOnly(r.DB())
	if err != nil {
		return false
	}
	defer st.Close()
	pending, _ := st.RestartPending(context.Background())
	return pending
}
