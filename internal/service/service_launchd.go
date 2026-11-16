package service

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/root"
)

var xmlReplacer = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")

func xmlEscape(s string) string { return xmlReplacer.Replace(s) }

// RenderLaunchd renders the agent plist exactly as TS does.
func RenderLaunchd(m Metadata, inv Invocation, rootDir, token string, otel map[string]string) string {
	args := append([]string{inv.Program}, inv.Arguments...)
	// The stderr path is under the canonical root (TS serviceStderrDirectory),
	// which resolves the nearest existing ancestor (/tmp is /private/tmp).
	stderrRoot := rootDir
	if c, err := root.Canonical(rootDir); err == nil {
		stderrRoot = c
	}
	stderr := filepath.Join(stderrRoot, "logs", "service-stderr.log")
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

func domain() string           { return "gui/" + strconv.Itoa(os.Getuid()) }
func target(m Metadata) string { return domain() + "/" + m.ServiceID }

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
