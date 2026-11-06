package testutil

import (
	"os"
	"path/filepath"
)

// Fataler is the part of testing.TB that RepoRoot needs. It isn't testing.TB itself, so this
// package doesn't import testing into the tools that use it.
type Fataler interface {
	Helper()
	Fatalf(format string, args ...any)
}

// RepoRoot is the repository's top directory: the nearest one above the working directory that
// holds .github. A test that reads a file outside the Go module starts from here, so a moved
// file fails the test instead of being skipped. It fails the test when there is no such
// directory.
func RepoRoot(t Fataler) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
		return ""
	}
	for {
		if info, err := os.Stat(filepath.Join(dir, ".github")); err == nil && info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no directory holding .github above the working directory")
			return ""
		}
		dir = parent
	}
}
