package contract

import (
	"encoding/json"
	"iter"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
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

// Pinned answers (step 7's R3c): the tests that compared each error answer, and the management
// script, with TS's pin Go's answers, recorded under -impl=both while TS still agreed.

var pinning sync.Map // top-level test name -> struct{}, while it pins answers

// pinnedGateways starts a test that pins answers: its own gateways on the golden fixture, TS (under
// -impl=both) and then the lab Go on the fixed host, so comparisons take TS first as they did with
// the shared targets. Where the goldens don't apply, the shared targets and no pins.
func pinnedGateways(t *testing.T) []*harness.Target {
	t.Helper()
	if harness.External() || !harness.PinValues(t) {
		return harness.Targets(t)
	}
	pinning.Store(t.Name(), struct{}{})
	t.Cleanup(func() { pinning.Delete(t.Name()) })
	launch := func(impl string, opts ...harness.Option) *harness.Target {
		t.Helper()
		tg, err := harness.Launch(impl, append([]harness.Option{harness.GoldenFixture()}, opts...)...)
		if err != nil {
			t.Fatalf("%s gateway: %v", impl, err)
		}
		t.Cleanup(tg.Stop)
		return tg
	}
	var tgs []*harness.Target
	if harness.Impl() == "both" {
		tgs = append(tgs, launch("ts"))
	}
	host := filepath.Join(fsguard.ScratchDir(t), "fixed-host.json")
	if err := fsguard.WriteFile(host, []byte(harness.FixedHostJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	return append(tgs, launch("go", harness.LabHost(host)))
}

var loopbackPort = regexp.MustCompile(`(127\.0\.0\.1|localhost):\d+`)

// pinError pins Go's error answer to label, where the test pins answers: its status, Allow header
// and envelope (type, code, param, message), with loopback ports normalized. name is the target's.
func pinError(t *testing.T, label, name string, r *harness.Resp) {
	t.Helper()
	top, _, _ := strings.Cut(t.Name(), "/")
	if _, ok := pinning.Load(top); !ok || name != "go" {
		return
	}
	e, _ := r.JSON(t)["error"].(map[string]any)
	v := map[string]any{"status": r.Status, "allow": r.Header.Get("Allow")}
	for _, k := range []string{"type", "code", "param", "message"} {
		v[k] = e[k]
	}
	harness.PinJSON(t, "error: "+label, normalizeAnswer(t, v))
}

// pinAnswer pins any of Go's answers (normalized as pinError's) where the test pins answers.
func pinAnswer(t *testing.T, label, name string, v any) {
	t.Helper()
	top, _, _ := strings.Cut(t.Name(), "/")
	if _, ok := pinning.Load(top); !ok || name != "go" {
		return
	}
	harness.PinJSON(t, label, normalizeAnswer(t, v))
}

func normalizeAnswer(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(loopbackPort.ReplaceAll(b, []byte("$1:<port>")), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// sortedCases ranges over a test's cases in label order, so a case that changes the gateway's
// state (activating a model, say) does it at the same point in every run.
func sortedCases(m map[string]string) iter.Seq2[string, string] {
	return func(yield func(string, string) bool) {
		for _, k := range slices.Sorted(maps.Keys(m)) {
			if !yield(k, m[k]) {
				return
			}
		}
	}
}
