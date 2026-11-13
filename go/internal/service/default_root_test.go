package service

import (
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/root"
)

// A test binary is refused the default root before any command runs: the safety net that keeps a
// test off the installed service (testing.Testing). A scratch root is allowed. HOME is a scratch
// directory here, so "the default root" is too, and nothing real is reached even if the refusal
// were gone.
func TestATestIsRefusedTheDefaultRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")
	if !testing.Testing() {
		t.Fatal("testing.Testing is false in a test binary")
	}
	g, calls := stoppedManager()
	for _, op := range []struct {
		name string
		do   func(*root.Root) (Inspection, error)
	}{
		{"StopWithin", g.StopWithin},
		{"Remove", g.Remove},
		// Start takes the root's lease before it refuses: under the scratch HOME, that is scratch too.
		{"Start", func(r *root.Root) (Inspection, error) { return g.Start(r, false, false) }},
	} {
		_, err := op.do(&root.Root{Dir: root.Default()})
		if err == nil || !strings.Contains(err.Error(), "A test tried to manage the service on the default root") {
			t.Errorf("%s on the default root: %v", op.name, err)
		}
	}
	if len(*calls) != 0 {
		t.Errorf("a refused operation ran commands: %q", *calls)
	}
	if err := refuseDefaultRootInTest(t.TempDir()); err != nil {
		t.Errorf("a scratch root: %v", err)
	}
}
