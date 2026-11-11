package harness

import (
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/testutil"
)

// TestMain pins that a test binary whose tests never ask for the shared targets launches none:
// this package's tests don't ask (the lifecycle test swaps in its own state), so Main must end
// with nothing launched.
func TestMain(m *testing.M) {
	code := Main(m)
	if shared.launched || len(shared.targets) != 0 {
		fmt.Fprintln(os.Stderr, "harness: the shared targets were launched, though no test asked for them")
		code = 1
	}
	os.Exit(code)
}

// TestMainStopsWhatItLaunched pins Main's lifecycle with a fake launch: no ask launches nothing;
// the first ask launches once and later asks reuse it; the targets are stopped once, when the
// tests have ended and not before.
func TestMainStopsWhatItLaunched(t *testing.T) {
	saved, savedLaunch := shared, launchShared
	t.Cleanup(func() { shared, launchShared = saved, savedLaunch })
	launches, stops := 0, 0
	launchShared = func() ([]*Target, error) {
		launches++
		return []*Target{{Name: "fake", stop: func() { stops++ }}}, nil
	}

	shared = &sharedTargets{}
	if code := run(func() int { return 0 }); code != 0 || launches != 0 || stops != 0 {
		t.Fatalf("no test asked: code %d, %d launches, %d stops", code, launches, stops)
	}

	shared = &sharedTargets{}
	code := run(func() int {
		a, b := Targets(t), Targets(t)
		if launches != 1 || len(a) != 1 || a[0] != b[0] {
			t.Errorf("two asks: %d launches, targets %v and %v", launches, a, b)
		}
		if stops != 0 {
			t.Errorf("stopped %d times while the tests were running", stops)
		}
		return 3
	})
	if code != 3 {
		t.Errorf("run returned %d, not the tests' code 3", code)
	}
	if stops != 1 {
		t.Errorf("stopped %d times after the tests ended, want 1", stops)
	}
}

// TestTargetsLaunchFailure: a failed launch fails every test that asks, with its error, and is
// not retried.
func TestTargetsLaunchFailure(t *testing.T) {
	saved, savedLaunch := shared, launchShared
	t.Cleanup(func() { shared, launchShared = saved, savedLaunch })
	launches := 0
	launchShared = func() ([]*Target, error) {
		launches++
		return nil, errors.New("launch go: planted")
	}
	shared = &sharedTargets{}
	for i := range 2 {
		r := &fatalRecorder{TB: t}
		if got := Targets(r); got != nil || !strings.Contains(r.msg, "launch go: planted") {
			t.Errorf("ask %d: targets %v, failure %q", i+1, got, r.msg)
		}
	}
	if launches != 1 {
		t.Errorf("%d launches, want 1", launches)
	}
}

// fatalRecorder records a Fatalf instead of ending the test.
type fatalRecorder struct {
	testing.TB
	msg string
}

func (r *fatalRecorder) Helper() {}

func (r *fatalRecorder) Fatalf(format string, args ...any) { r.msg = fmt.Sprintf(format, args...) }

// Floors for TestNoProductionImports: what the walk reads now (341 files in 55 packages at step
// 7's R2). It is the one guard of its kind (O3 drops treewalk's), so a floor below that would let
// files drop out unnoticed (ci-rules 8). Raise them as the product grows.
const (
	minProductFiles    = 341
	minProductPackages = 55
)

// TestNoProductionImports keeps test code out of the product: no non-test .go file outside
// go/test and go/tools imports a package under go/test (the harness and fsguard; test/lab after
// step 7's R4). Every package under go/test registers test flags or writes only to scratch, so
// none belongs in a binary. The walk fails below its floors, so one that reads nothing can't pass.
func TestNoProductionImports(t *testing.T) {
	goDir := filepath.Join(testutil.RepoRoot(t), "go")
	const testPrefix = "github.com/timsexperiments/LocalBase/go/test/"
	files, packages := 0, map[string]bool{}
	err := filepath.WalkDir(goDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(goDir, path)
		if d.IsDir() {
			if rel == "test" || rel == "tools" || d.Name() == "testdata" || (rel != "." && strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		files++
		packages[filepath.Dir(rel)] = true
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); strings.HasPrefix(p, testPrefix) {
				t.Errorf("%s imports %s: test code in the product", rel, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < minProductFiles || len(packages) < minProductPackages {
		t.Fatalf("read %d files in %d packages, under the floors (%d, %d)", files, len(packages), minProductFiles, minProductPackages)
	}
	t.Logf("read %d non-test files in %d packages", files, len(packages))
}
