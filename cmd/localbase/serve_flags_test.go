package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serve refuses a --log-level it doesn't know, before it starts anything, as logs --level does,
// instead of running at info (review 01-M3). The bad --port makes main fail fast too, but on the port.
func TestServeRefusesAnUnknownLogLevel(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	root := filepath.Join(t.TempDir(), "root")
	for _, level := range []string{"warning", "err", "INFO "} {
		code, out := runApp(t, "--root", root, "serve", "--log-level", level, "--port", "0")
		want := `log-level: Invalid option: expected one of "debug"|"info"|"warn"|"error"`
		if code != 2 || !strings.Contains(out, want) {
			t.Errorf("--log-level %q: exit %d, %s", level, code, out)
		}
	}
	// What worked before still does: these get past the level to the port.
	for _, level := range []string{"INFO", "warn", "debug", "error"} {
		if code, out := runApp(t, "--root", root, "serve", "--log-level", level, "--port", "0"); code != 2 || !strings.Contains(out, "--port must be") {
			t.Errorf("--log-level %q: exit %d, %s", level, code, out)
		}
	}
}

// A runtime's port and the connection caps are checked with the other flags, before the root is
// opened: before, they were read only once the gateway had its lock and its backends. A runtime
// that is off takes no port, as before.
func TestServeChecksEveryFlagBeforeOpeningTheRoot(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--tts-port", "70000"}, "--tts-port must be from 1 to 65535"},
		{[]string{"--stt-port", "0"}, "--stt-port must be from 1 to 65535"},
		{[]string{"--max-connections=-1"}, "--max-connections must be from 0 to 10000000"},
		{[]string{"--max-connections-per-ip", "x"}, "max-connections-per-ip"},
	} {
		root := filepath.Join(t.TempDir(), "root")
		code, out := runApp(t, append([]string{"--root", root, "serve", "--no-llm"}, tc.args...)...)
		if code != 2 || !strings.Contains(out, tc.want) {
			t.Errorf("%q: exit %d, %s", tc.args, code, out)
		}
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Errorf("%q: the root was made (%v)", tc.args, err)
		}
	}
	root := noAuthRoot(t)
	if code, out := runApp(t, "--root", root, "serve", "--no-llm", "--no-tts", "--tts-port", "70000", "--port", busyPort(t)); code == 0 || strings.Contains(out, "--tts-port") {
		t.Errorf("--no-tts with a bad port: exit %d, %s", code, out)
	}
}
