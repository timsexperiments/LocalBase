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

// The pinned tests here, TestModelsListPinned and TestModelMetadata, compared Go's
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

// Pinned answers: the tests that compared each error answer, the model management state and
// script, and the voice lifecycle with TS's pin Go's answers instead. Step 7's R3c recorded them
// under -impl=both while TS still agreed (on the recording branch named in R3c's description).

var pinning sync.Map // top-level test name -> struct{}, while it pins answers

// pinnedGateways starts a test that pins answers: a gateway of its own, the lab Go on the fixed
// host and the golden fixture, so the answers don't turn on the machine or on what other tests did.
// Against an external gateway (-url) it pins nothing.
func pinnedGateways(t *testing.T) []*harness.Target {
	t.Helper()
	if harness.External() {
		return harness.Targets(t)
	}
	harness.PinValues(t)
	pinning.Store(t.Name(), struct{}{})
	t.Cleanup(func() { pinning.Delete(t.Name()) })
	host := filepath.Join(fsguard.ScratchDir(t), "fixed-host.json")
	if err := fsguard.WriteFile(host, []byte(harness.FixedHostJSON()), 0o600); err != nil {
		t.Fatal(err)
	}
	tg, err := harness.Launch(harness.GoldenFixture(), harness.LabHost(host))
	if err != nil {
		t.Fatalf("lab gateway: %v", err)
	}
	t.Cleanup(tg.Stop)
	return []*harness.Target{tg}
}

var loopbackPort = regexp.MustCompile(`(127\.0\.0\.1|localhost):\d+`)

// pinError pins the error answer to label, where the test pins answers: its status, Allow header
// and envelope (type, code, param, message), with loopback ports normalized.
func pinError(t *testing.T, label string, r *harness.Resp) {
	t.Helper()
	e, _ := r.JSON(t)["error"].(map[string]any)
	v := map[string]any{"status": r.Status, "allow": r.Header.Get("Allow")}
	for _, k := range []string{"type", "code", "param", "message"} {
		v[k] = e[k]
	}
	pinAnswer(t, "error: "+label, v)
}

// pinAnswer pins any answer, normalized as pinError's, where the test pins answers.
func pinAnswer(t *testing.T, label string, v any) {
	t.Helper()
	top, _, _ := strings.Cut(t.Name(), "/")
	if _, ok := pinning.Load(top); !ok {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(loopbackPort.ReplaceAll(b, []byte("$1:<port>")), &out); err != nil {
		t.Fatal(err)
	}
	harness.PinJSON(t, label, out)
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
