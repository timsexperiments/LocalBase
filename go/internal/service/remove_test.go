package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/root"
)

// stoppedManager is a Manager standing in for launchctl or systemctl with the service installed and
// not running, accepting every change asked of it. calls is every command it was asked to run.
func stoppedManager() (g Manager, calls *[][]string) {
	calls = new([][]string)
	g.runner = func(args ...string) (result, error) {
		*calls = append(*calls, append([]string(nil), args...))
		joined := strings.Join(args[1:], " ")
		switch {
		case len(args) >= 3 && args[1] == "print" && strings.Count(args[2], "/") == 2:
			return result{code: 113, stderr: "Could not find service"}, nil // launchd: not loaded
		case strings.Contains(joined, "--user show "):
			return result{stdout: "LoadState=not-found\nActiveState=inactive\nSubState=dead\nMainPID=0\nNRestarts=0\n"}, nil
		case strings.Contains(joined, "is-enabled"):
			return result{code: 1, stdout: "disabled\n"}, nil
		}
		return result{}, nil
	}
	return g, calls
}

// installed is a scratch root with a service definition and manifest, under a scratch home, and the
// stopped Manager to remove it with.
func installed(t *testing.T) (*root.Root, Metadata, Manager) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root can remove from a directory it can't write to")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	r, err := root.Init(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := MetadataFor(r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{m.DefinitionPath, manifestPath(r.Dir)} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	g, _ := stoppedManager()
	return r, m, g
}

// keep makes a file's directory refuse to let it go, until the test ends.
func keep(t *testing.T, path string) {
	t.Helper()
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

// A definition that stays would start the service again at the next login, so a removal that
// leaves it says so, and doesn't report the service removed.
func TestRemovingAServiceSaysItsDefinitionStayed(t *testing.T) {
	r, m, g := installed(t)
	keep(t, m.DefinitionPath)
	_, err := g.Remove(r)
	want := "Stopped the LocalBase service, but could not remove its definition, " + m.DefinitionPath + ": permission denied. " +
		"Remove that file by hand, or the service may start again at the next login."
	if err == nil || err.Error() != want {
		t.Fatalf("got  %v\nwant %s", err, want)
	}
	if _, err := os.Stat(m.DefinitionPath); err != nil {
		t.Errorf("the definition should still be there: %v", err)
	}
}

// A manifest that stays keeps LocalBase reporting a service, so that is said too.
func TestRemovingAServiceSaysItsManifestStayed(t *testing.T) {
	r, m, g := installed(t)
	keep(t, manifestPath(r.Dir))
	_, err := g.Remove(r)
	want := "Stopped the LocalBase service, but could not remove " + manifestPath(r.Dir) + ": permission denied. " +
		"Remove that file by hand, or LocalBase will still report a service here."
	if err == nil || err.Error() != want {
		t.Fatalf("got  %v\nwant %s", err, want)
	}
	if _, err := os.Stat(m.DefinitionPath); err == nil {
		t.Errorf("the definition should be gone")
	}
}

// Files already gone are what removing wants, and aren't errors.
func TestRemovingAServiceWithNothingLeftIsFine(t *testing.T) {
	r, m, g := installed(t)
	for _, p := range []string{m.DefinitionPath, manifestPath(r.Dir)} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := g.Remove(r); err != nil {
		t.Fatalf("remove: %v", err)
	}
}

// Every command a removal runs goes through the Manager it was asked of, in this order, for the
// stopped manager's answers: stopManaged's inspection, stopManager's own, the disable (no bootout or
// stop: nothing is loaded or active), one poll that finds it stopped, systemd's reload after the
// files go, and the final Inspect. A path that made its own Manager would run the real launchctl
// or systemctl, and its commands would be missing here, even where the real one answers as the fake
// does (launchctl print of a scratch label: 113, not found).
func TestRemoveRunsEveryCommandThroughItsManager(t *testing.T) {
	r, m, _ := installed(t)
	g, calls := stoppedManager()
	if _, err := g.Remove(r); err != nil {
		t.Fatalf("remove: %v", err)
	}
	var inspect, want [][]string
	if m.Manager == "launchd" {
		inspect = [][]string{{"print", domain()}, {"print", target(m)}, {"print-disabled", domain()}}
	} else {
		inspect = [][]string{{"--user", "show", m.UnitName,
			"--property=LoadState,ActiveState,SubState,MainPID,ExecMainCode,ExecMainStatus,NRestarts", "--no-pager"},
			{"--user", "is-enabled", m.UnitName}}
	}
	want = append(want, inspect...) // stopManaged
	want = append(want, inspect...) // stopManager, before
	if m.Manager == "launchd" {
		want = append(want, []string{"disable", target(m)})
	} else {
		want = append(want, []string{"--user", "disable", m.UnitName})
	}
	want = append(want, inspect...) // the poll: stopped
	if m.Manager != "launchd" {
		want = append(want, []string{"--user", "daemon-reload"})
	}
	want = append(want, inspect...) // the final Inspect
	for i := range want {
		want[i] = append([]string{executable(m)}, want[i]...)
	}
	got := *calls
	if len(got) != len(want) {
		t.Fatalf("%d commands, want %d:\n got  %q\n want %q", len(got), len(want), got, want)
	}
	for i := range want {
		if strings.Join(got[i], " ") != strings.Join(want[i], " ") {
			t.Errorf("command %d: %q, want %q", i, got[i], want[i])
		}
	}
}
