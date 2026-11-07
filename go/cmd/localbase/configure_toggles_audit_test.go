package main

import (
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Turning an import opt-in on or off is audited, once per toggle that actually changes, after the
// save; a command that leaves it as it was, or is refused, says nothing about it.
func TestConfigureAuditsAChangedImportToggle(t *testing.T) {
	for _, toggle := range []struct{ flag, key string }{
		{"allow-private-imports", store.PrivateImportsKey},
		{"allow-local-path-imports", store.LocalPathImportsKey},
	} {
		t.Run(toggle.flag, func(t *testing.T) {
			t.Setenv("LOCALBASE_ROOT", "")
			root := filepath.Join(t.TempDir(), "root")
			if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
				t.Fatalf("configure: %s", out)
			}
			// The toggle lines a command wrote, after checking configure's own line comes first.
			toggles := func(args ...string) []auditLine {
				t.Helper()
				before := len(auditLines(t, root))
				if code, out := runApp(t, append([]string{"--root", root, "configure", "--no-create-key"}, args...)...); code != 0 {
					t.Fatalf("configure %v: %s", args, out)
				}
				lines := auditLines(t, root)[before:]
				if len(lines) == 0 || lines[0].Action != "configure" {
					t.Fatalf("configure %v: %+v", args, lines)
				}
				return lines[1:]
			}
			want := func(got []auditLine, value string) {
				t.Helper()
				if len(got) != 1 {
					t.Fatalf("%d toggle lines, want 1: %+v", len(got), got)
				}
				l := got[0]
				if l.Event != "audit.config-changed" || l.Action != "set-setting" || l.ActorKind != "cli" ||
					l.TargetKind != "setting" || l.TargetID != toggle.key || l.Setting != toggle.key || l.Value != value {
					t.Errorf("%+v, want %s set to %s", l, toggle.key, value)
				}
			}

			// Unset is off, so turning it off changes nothing.
			if got := toggles("--no-" + toggle.flag); len(got) != 0 {
				t.Errorf("off when it was unset: %+v", got)
			}
			want(toggles("--"+toggle.flag), "on")
			if got := toggles("--" + toggle.flag); len(got) != 0 {
				t.Errorf("on when it was on: %+v", got)
			}
			if got := toggles(); len(got) != 0 {
				t.Errorf("not given: %+v", got)
			}

			// A refused command writes nothing, so it audits nothing.
			before := len(auditLines(t, root))
			if code, _ := runApp(t, "--root", root, "configure", "--no-create-key", "--no-"+toggle.flag, "--max-connections", "abc"); code == 0 {
				t.Fatal("--max-connections abc was accepted")
			}
			if lines := auditLines(t, root)[before:]; len(lines) != 0 {
				t.Errorf("a refused command audited: %+v", lines)
			}

			want(toggles("--no-"+toggle.flag), "off")
		})
	}
}
