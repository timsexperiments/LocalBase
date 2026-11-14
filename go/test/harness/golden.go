package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// Goldens: a test that used to compare Go with TS, and checked nothing else, pins Go's results
// instead (step 7's R3a recorded them while TS still agreed; R3b removed TS). The system package
// pins CLI transcripts; PinValues and PinJSON pin values, in each package's
// testdata/golden/<platform>/<Test>.values.json, one entry per name.
//
// Goldens are written only with LOCALBASE_UPDATE_GOLDEN=1, and only when the test passed
// otherwise. A golden is then read in review like any other source file: its diff is the change
// in what Go prints.
//
// A pinned test's Go runs as a localbase_lab build on FixedHostJSON's host, so what follows the
// host's memory (fit verdicts, what fits) is the same on every runner. The lab build differs from
// the release only in that seam, which TestLabSeamFilesAreTagged holds to localbase_lab files.
// A pinned test that launches gateways launches them on GoldenFixture, which no variable changes.
//
// Each golden platform has its own goldens: darwin/arm64 (recorded on a Mac) and linux/amd64
// (recorded in a linux/amd64 container), because the platform changes the catalog's targets,
// doctor's placement and the service manager.

// goldenPlatforms is the platforms with goldens, and each one's fixed host: 64 GiB, unified
// with a GPU working set on Apple silicon, a plain host pool on Linux.
var goldenPlatforms = map[string]string{
	"darwin/arm64": `{"capacityBytes":68719476736,"unified":true,"availableBytes":42949672960,"pressure":"normal","gpuWorkingSetBytes":51539607552}`,
	"linux/amd64":  `{"capacityBytes":68719476736,"unified":false,"availableBytes":42949672960,"pressure":"normal"}`,
}

// FixedHostJSON is this platform's fixed host for the pinned tests (goldenPlatforms).
func FixedHostJSON() string { return goldenPlatforms[goruntime.GOOS+"/"+goruntime.GOARCH] }

// GoldenDir is this platform's directory of goldens, under the package's testdata/golden.
func GoldenDir() string { return goruntime.GOOS + "-" + goruntime.GOARCH }

// Pins starts a pinned test. It skips where there are no goldens: another platform (a platform
// absence, named in go/test/contract), or real backends on PATH.
func Pins(t *testing.T) {
	t.Helper()
	// A platform absence (platformAbsences in go/test/contract).
	if goldenPlatforms[goruntime.GOOS+"/"+goruntime.GOARCH] == "" {
		t.Skipf("pins Go's results: the goldens are darwin/arm64's and linux/amd64's; %s/%s would need goldens of its own", goruntime.GOOS, goruntime.GOARCH)
	}
	// The pinned CLI runs see the real backends on PATH (Guarded) only under this variable.
	if os.Getenv(allowRealModelsEnv) == "1" {
		t.Skip("pins Go's results: " + allowRealModelsEnv + " lets the CLI see real backends, and the goldens were recorded without them")
	}
}

type valuePins struct {
	mu     sync.Mutex
	golden map[string]json.RawMessage // nil when recording
	got    map[string]json.RawMessage
}

var pinned sync.Map // top-level test name -> *valuePins

// PinValues starts a test's value goldens (Pins first); PinJSON then pins each value, from the
// test or any of its subtests. A golden name the test never pins fails it at the end.
func PinValues(t *testing.T) {
	t.Helper()
	Pins(t)
	update := os.Getenv(fsguard.UpdateGoldenEnv) == "1"
	name := filepath.Join(GoldenDir(), t.Name()+".values.json")
	file := filepath.Join("testdata", "golden", name)
	p := &valuePins{got: map[string]json.RawMessage{}}
	if !update {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("no golden values for %s (%v): record them with %s=1", t.Name(), err, fsguard.UpdateGoldenEnv)
		}
		if err := json.Unmarshal(b, &p.golden); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
	}
	pinned.Store(t.Name(), p)
	t.Cleanup(func() {
		pinned.Delete(t.Name())
		if update {
			if t.Failed() {
				t.Logf("%s not recorded: the test failed", file)
				return
			}
			b, _ := json.MarshalIndent(p.got, "", "  ")
			if err := fsguard.WriteGolden(name, append(b, '\n')); err != nil {
				t.Error(err)
			}
			return
		}
		var missing []string
		for name := range p.golden {
			if _, ok := p.got[name]; !ok {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("%s pins values the test no longer produces: %v", file, missing)
		}
	})
}

// PinJSON pins one of the test's values: under recording it keeps the value, otherwise it
// compares the value with the golden's.
func PinJSON(t *testing.T, name string, got any) {
	t.Helper()
	top, _, _ := strings.Cut(t.Name(), "/")
	v, ok := pinned.Load(top)
	if !ok {
		t.Fatalf("PinJSON(%q) without PinValues", name)
	}
	p := v.(*valuePins)
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, dup := p.got[name]; dup {
		t.Fatalf("PinJSON(%q) twice", name)
	}
	p.got[name] = b
	if p.golden == nil {
		return // recording
	}
	want, ok := p.golden[name]
	if !ok {
		t.Errorf("no golden value %q: record it with %s=1", name, fsguard.UpdateGoldenEnv)
		return
	}
	var a, c any
	if err := json.Unmarshal(want, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if d := DiffJSON("", a, c, nil); len(d) > 0 {
		if len(d) > 12 {
			d = append(slices.Clone(d[:12]), "...")
		}
		t.Errorf("%s differs from its golden (golden vs now):\n  %s\nIf the change is intended, record again with %s=1.",
			name, strings.Join(d, "\n  "), fsguard.UpdateGoldenEnv)
	}
}

// PruneJSON is v without the paths drop names, in DiffJSON's path syntax (".key", "[i]"): for a
// golden that leaves out what varies from run to run.
func PruneJSON(v any, path string, drop func(string) bool) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			if p := path + "." + k; !drop(p) {
				out[k] = PruneJSON(e, p, drop)
			}
		}
		return out
	case []any:
		out := make([]any, 0, len(x))
		for i, e := range x {
			if p := fmt.Sprintf("%s[%d]", path, i); !drop(p) {
				out = append(out, PruneJSON(e, p, drop))
			}
		}
		return out
	}
	return v
}
