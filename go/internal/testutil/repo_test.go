package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestRepoRootFindsTheRepository(t *testing.T) {
	root := RepoRoot(t)
	for _, p := range []string{".github/workflows", "go/go.mod"} {
		if _, err := os.Stat(filepath.Join(root, p)); err != nil {
			t.Errorf("%s isn't the repository: %v", root, err)
		}
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
}
