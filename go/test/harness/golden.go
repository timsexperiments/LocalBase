package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// Goldens: before TypeScript is retired (step 7's R3a), a test that compared Go with TS and
// checked nothing else also pins Go's results, so it keeps checking something once TS is gone.
// The system package pins CLI transcripts; PinValues and PinJSON pin values, in each package's
// testdata/golden/<Test>.values.json, one entry per name.
//
// Goldens are written only under -impl=both with LOCALBASE_UPDATE_GOLDEN=1, and only when the test
// passed: TS and Go then agreed on everything the test compares, so each golden is TS's answer
// too. Every other run where Pins is true compares with the goldens.
//
// A pinned test's Go runs as a localbase_lab build on FixedHostJSON's host, so what follows the
// host's memory (fit verdicts, what fits) is the same on every runner. The lab build differs from
// the release only in that seam, which TestLabSeamFilesAreTagged holds to localbase_lab files.
// A pinned test that launches gateways launches them on GoldenFixture, which no variable changes.

// FixedHostJSON is the pinned tests' host: 64 GiB of unified memory, as on the Mac the goldens
// were recorded on, where Go on this host and TS on the real one agreed.
const FixedHostJSON = `{"capacityBytes":68719476736,"unified":true,"availableBytes":42949672960,"pressure":"normal","gpuWorkingSetBytes":51539607552}`

const (
	goldenOS, goldenArch = "darwin", "arm64"
	goldenHostBytes      = int64(64) << 30
)

// machineBytes is this machine's memory (hw.memsize), read once; 0 when it can't be read.
var machineBytes = sync.OnceValue(func() int64 {
	out, err := exec.Command("/usr/sbin/sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return n
})

// unpinnedBecause says why this run, on the goldens' platform, can't use them, or "".
func unpinnedBecause() string {
	// The pinned CLI runs see the real backends on PATH (Guarded) only under this variable.
	if os.Getenv(allowRealModelsEnv) == "1" {
		return allowRealModelsEnv + " lets the CLI see real backends, and the goldens were recorded without them"
	}
	// TS reads this machine's memory, so it agrees with Go on the fixed host only on a machine
	// of the same size.
	if Impl() == "both" && machineBytes() != goldenHostBytes {
		return fmt.Sprintf("under -impl=both TS reads this machine's memory (%d bytes), and the goldens' fixed host is 64 GiB", machineBytes())
	}
	return ""
}

// Pins starts a pinned test and says whether it pins. Under -impl=go it pins, or skips where the
// goldens don't apply (another platform, real backends on PATH). Under -impl=both it needs
// TS and pins where the goldens apply and TS can agree with the fixed host (a 64 GiB machine);
// elsewhere it only compares Go with TS, as before the pins, with Go on this machine. -impl=ts
// skips.
func Pins(t *testing.T) bool {
	t.Helper()
	update := os.Getenv(fsguard.UpdateGoldenEnv) == "1"
	if update && Impl() != "both" {
		t.Fatalf("goldens are recorded under -impl=both only, where TS has to agree with Go")
	}
	switch Impl() {
	case "both":
		RequireTS(t)
	case "go":
	default:
		t.Skip("pins Go's own results")
	}
	// A platform absence (platformAbsences in go/test/contract): the platform changes the
	// catalog's targets and doctor's placement.
	if goruntime.GOOS != goldenOS || goruntime.GOARCH != goldenArch {
		why := fmt.Sprintf("the goldens are darwin/arm64's, where go-dual ran these tests; %s/%s would need goldens of its own", goruntime.GOOS, goruntime.GOARCH)
		if Impl() == "go" {
			t.Skip("pins Go's results: " + why)
		}
		return comparesOnly(t, why)
	}
	if why := unpinnedBecause(); why != "" {
		if Impl() == "go" {
			t.Skip("pins Go's results: " + why)
		}
		return comparesOnly(t, why)
	}
	return true
}

// comparesOnly is Pins under -impl=both where the goldens don't apply.
func comparesOnly(t *testing.T, why string) bool {
	t.Helper()
	if os.Getenv(fsguard.UpdateGoldenEnv) == "1" {
		t.Fatalf("goldens cannot be recorded here: %s", why)
	}
	t.Logf("comparing with TS without the goldens: %s", why)
	return false
}

type valuePins struct {
	mu     sync.Mutex
	off    bool                       // Pins said no: nothing is pinned
	golden map[string]json.RawMessage // nil when recording
	got    map[string]json.RawMessage
}

var pinned sync.Map // top-level test name -> *valuePins

// PinValues starts a test's value goldens (Pins, whose answer it returns); PinJSON then pins each
// value, from the test or any of its subtests. A golden name the test never pins fails it at the
// end.
func PinValues(t *testing.T) bool {
	t.Helper()
	if !Pins(t) {
		pinned.Store(t.Name(), &valuePins{off: true})
		t.Cleanup(func() { pinned.Delete(t.Name()) })
		return false
	}
	update := os.Getenv(fsguard.UpdateGoldenEnv) == "1"
	file := filepath.Join("testdata", "golden", t.Name()+".values.json")
	p := &valuePins{got: map[string]json.RawMessage{}}
	if !update {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("no golden values for %s (%v): record them with -impl=both and %s=1", t.Name(), err, fsguard.UpdateGoldenEnv)
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
				t.Logf("%s not recorded: the test failed, so TS and Go did not agree", file)
				return
			}
			b, _ := json.MarshalIndent(p.got, "", "  ")
			if err := fsguard.WriteGolden(t.Name()+".values.json", append(b, '\n')); err != nil {
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
	return true
}

// PinJSON pins one of the test's values: under recording it keeps the value, otherwise it
// compares the value with the golden's. It does nothing when PinValues said no.
func PinJSON(t *testing.T, name string, got any) {
	t.Helper()
	top, _, _ := strings.Cut(t.Name(), "/")
	v, ok := pinned.Load(top)
	if !ok {
		t.Fatalf("PinJSON(%q) without PinValues", name)
	}
	p := v.(*valuePins)
	if p.off {
		return
	}
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
		t.Errorf("no golden value %q: record it with -impl=both and %s=1", name, fsguard.UpdateGoldenEnv)
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
		t.Errorf("%s differs from its golden (golden vs now):\n  %s\nIf the change is intended, record again with -impl=both and %s=1.",
			name, strings.Join(d, "\n  "), fsguard.UpdateGoldenEnv)
	}
}

// PruneJSON is v without the paths drop names, in DiffJSON's path syntax (".key", "[i]"): for a
// golden that leaves out what a comparison with TS skipped as volatile.
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
