package contract

import (
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/test/harness"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// The pinned tests here, TestDifferentialModels and TestDifferentialModelMetadata, compared Go's
// model metadata with TS's and tested nothing else, so before TypeScript is retired (step 7's R3a)
// they also pin Go's answers as value goldens (harness.PinValues; harness.Pins says where).
//
// Where a test pins, it launches gateways of its own on harness.GoldenFixture, which no variable
// changes: Go always, and TS under -impl=both. What /_localbase/models says about fit follows the
// host's memory, so its Go gateway is a localbase_lab build on harness.FixedHostJSON's 64 GiB
// unified host (harness.LabHost), as the system package's pinned CLI runs are. The lab build
// differs from the release only in that host seam, which TestLabSeamFilesAreTagged holds to
// localbase_lab files. Where a test doesn't pin, it compares the shared TS and Go targets, as
// before the pins.

// pinnedTargets starts a pinned test: TS (nil under -impl=go) and the Go gateway to compare it
// with, which is the one pinned when the test pins. Where it pins, both are its own, on the golden
// fixture (harness.GoldenFixture); where it doesn't, they are the shared targets.
func pinnedTargets(t *testing.T) (ts, gw *harness.Target) {
	t.Helper()
	if harness.External() {
		t.Skip("pins gateways the harness launches; -url names one it doesn't")
	}
	if !harness.PinValues(t) {
		for _, tg := range harness.Targets(t) {
			switch tg.Name {
			case "ts":
				ts = tg
			case "go":
				gw = tg
			}
		}
		return ts, gw
	}
	launch := func(impl string, opts ...harness.Option) *harness.Target {
		t.Helper()
		tg, err := harness.Launch(impl, append([]harness.Option{harness.GoldenFixture()}, opts...)...)
		if err != nil {
			t.Fatalf("%s gateway: %v", impl, err)
		}
		t.Cleanup(tg.Stop)
		return tg
	}
	if harness.Impl() == "both" {
		ts = launch("ts")
	}
	host := filepath.Join(fsguard.ScratchDir(t), "fixed-host.json")
	if err := fsguard.WriteFile(host, []byte(harness.FixedHostJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	return ts, launch("go", harness.LabHost(host))
}
