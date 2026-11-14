package contract

import (
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/test/harness"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// The pinned tests here, TestDifferentialModels and TestDifferentialModelMetadata, compared Go's
// model metadata with TS's and tested nothing else, so step 7's R3a pinned Go's answers as value
// goldens (harness.PinValues; harness.Pins says where) while TS still agreed, and R3b removed TS.
//
// Each launches a gateway of its own on harness.GoldenFixture, which no variable changes. What
// /_localbase/models says about fit follows the host's memory, so the gateway is a localbase_lab
// build on harness.FixedHostJSON's 64 GiB host (harness.LabHost), as the system package's pinned
// CLI runs are. The lab build differs from the release only in that host seam, which
// TestLabSeamFilesAreTagged holds to localbase_lab files.

// pinnedTarget starts a pinned test and launches its gateway (see the comment at the top).
func pinnedTarget(t *testing.T) *harness.Target {
	t.Helper()
	if harness.External() {
		t.Skip("pins a gateway the harness launches; -url names one it doesn't")
	}
	harness.PinValues(t)
	host := filepath.Join(fsguard.ScratchDir(t), "fixed-host.json")
	if err := fsguard.WriteFile(host, []byte(harness.FixedHostJSON()), 0o600); err != nil {
		t.Fatal(err)
	}
	tg, err := harness.Launch(harness.GoldenFixture(), harness.LabHost(host))
	if err != nil {
		t.Fatalf("lab gateway: %v", err)
	}
	t.Cleanup(tg.Stop)
	return tg
}
