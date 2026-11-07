package main

import (
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
