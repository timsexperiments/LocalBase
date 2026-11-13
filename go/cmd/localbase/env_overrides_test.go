package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// A LOCALBASE_* override that isn't a number in range is bad input (exit 2), in its own words,
// before doctor reports anything.
func TestABadEnvironmentOverrideIsBadInput(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	root := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	for name, value := range map[string]string{"LOCALBASE_PORT": "0", "LOCALBASE_STT_PORT": "x", "LOCALBASE_CTX_SIZE": "100"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, value)
			code, out := runApp(t, "--root", root, "doctor")
			want := map[string]string{"LOCALBASE_PORT": "LOCALBASE_PORT: must be an integer from 1 to 65535",
				"LOCALBASE_STT_PORT": "LOCALBASE_STT_PORT: must be an integer from 1 to 65535",
				"LOCALBASE_CTX_SIZE": "LOCALBASE_CTX_SIZE: must be an integer from 2048 to 2147483647"}[name]
			if code != 2 || !strings.Contains(out, want) {
				t.Errorf("exit %d, %s; want exit 2 and %q", code, out, want)
			}
		})
	}
}
