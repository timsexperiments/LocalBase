package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// A LOCALBASE_* override that isn't a number in range is bad input (exit 2), in its own words,
// before doctor reports anything or serve starts anything.
func TestABadEnvironmentOverrideIsBadInput(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	root := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	for name, value := range map[string]string{"LOCALBASE_PORT": "0", "LOCALBASE_STT_PORT": "x", "LOCALBASE_CTX_SIZE": "100"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, value)
			want := map[string]string{"LOCALBASE_PORT": "LOCALBASE_PORT: must be an integer from 1 to 65535",
				"LOCALBASE_STT_PORT": "LOCALBASE_STT_PORT: must be an integer from 1 to 65535",
				"LOCALBASE_CTX_SIZE": "LOCALBASE_CTX_SIZE: must be an integer from 2048 to 2147483647"}[name]
			// serve refuses it the same way, after opening the root and before it binds.
			for _, args := range [][]string{{"doctor"}, {"serve", "--no-llm", "--port", busyPort(t)}} {
				code, out := runApp(t, append([]string{"--root", root}, args...)...)
				if code != 2 || !strings.Contains(out, want) {
					t.Errorf("%s: exit %d, %s; want exit 2 and %q", args[0], code, out, want)
				}
			}
		})
	}
}
