package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

// doctor prints this computer's verdict (memory.HardwareStatus) on its own line.
func TestDoctorSaysTheHardwareStatus(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	root := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	code, out := runApp(t, "--root", root, "doctor")
	want := "Status: " + memory.HardwareStatus(memory.DetectSpecs()) + "\n"
	if code != 0 || !strings.Contains(out, want) {
		t.Errorf("exit %d; want the line %q in:\n%s", code, want, out)
	}
}
