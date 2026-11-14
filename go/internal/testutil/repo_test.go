package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoRootFindsTheRepository(t *testing.T) {
	root := RepoRoot(t)
	if _, err := os.Stat(filepath.Join(root, ".github", "workflows")); err != nil {
		t.Errorf("%s isn't the repository: %v", root, err)
	}
}

// The module is this repository's: its go.mod is in or under the repository's top, and is the
// LocalBase module.
func TestModuleRootFindsTheModule(t *testing.T) {
	root, module := RepoRoot(t), ModuleRoot(t)
	rel, err := filepath.Rel(root, module)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("module %s isn't inside the repository %s", module, root)
	}
	b, err := os.ReadFile(filepath.Join(module, "go.mod"))
	if err != nil || !strings.HasPrefix(string(b), "module github.com/timsexperiments/LocalBase") {
		t.Errorf("%s/go.mod isn't LocalBase's: %v", module, err)
	}
}

// fakeT records a failure instead of stopping the test, so the failure itself can be checked.
type fakeT struct{ failed string }

func (f *fakeT) Helper() {}
func (f *fakeT) Fatalf(format string, args ...any) {
	f.failed = fmt.Sprintf(format, args...)
}

func TestRepoRootFailsOutsideARepository(t *testing.T) {
	t.Chdir(t.TempDir())
	f := &fakeT{}
	if got := RepoRoot(f); got != "" || f.failed == "" {
		t.Errorf("outside a repository: got %q, failure %q", got, f.failed)
	}
	f = &fakeT{}
	if got := ModuleRoot(f); got != "" || f.failed == "" {
		t.Errorf("outside a module: got %q, failure %q", got, f.failed)
	}
}
