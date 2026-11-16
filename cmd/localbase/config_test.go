package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A database that can't be read isn't one with nothing in it: its settings aren't the defaults, and a
// command that shows settings says it couldn't read them.
func TestUnreadableSettingsAreNotShownAsDefaults(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	root := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	db := filepath.Join(root, "local-base.db")
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(db + suffix) // only there if a process left them
	}
	if err := os.WriteFile(db, []byte("this isn't a database, and is long enough that SQLite reads it as a header to reject"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"doctor"}, {"models", "show", "qwen3.5-4b-q4_k_m"}} {
		code, out := runApp(t, append([]string{"--root", root}, args...)...)
		if code == 0 {
			t.Errorf("%v: exit 0, showing %.300q", args, out)
		}
		if !strings.Contains(out, "Could not read") && !strings.Contains(out, "never finished") {
			t.Errorf("%v: %.300q", args, out)
		}
		if strings.Contains(out, "Parallel Slots: auto") {
			t.Errorf("%v shows the defaults as this root's: %.300q", args, out)
		}
	}
}
