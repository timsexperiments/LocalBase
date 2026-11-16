package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

type fatalRecorder struct{ failed string }

func (f *fatalRecorder) Helper() {}
func (f *fatalRecorder) Fatalf(format string, args ...any) {
	f.failed = fmt.Sprintf(format, args...)
}

func writeTree(t *testing.T, files ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("package x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// The packages under a directory are read too, and tests, testdata and hidden directories aren't.
func TestGoSourcesReadsThePackagesUnderIt(t *testing.T) {
	root := writeTree(t, "a.go", "a_test.go", "sub/b.go", "sub/deeper/c.go", "testdata/d.go", ".hidden/e.go", "notes.md")
	got := GoSources(t, root, 3)
	var rel []string
	for _, f := range got {
		r, _ := filepath.Rel(root, f)
		rel = append(rel, filepath.ToSlash(r))
	}
	slices.Sort(rel)
	if want := []string{"a.go", "sub/b.go", "sub/deeper/c.go"}; !slices.Equal(rel, want) {
		t.Errorf("read %v, want %v", rel, want)
	}
}

// Fewer packages than expected fails, so a check can't quietly read less.
func TestGoSourcesFailsBelowItsMinimum(t *testing.T) {
	root := writeTree(t, "a.go", "sub/b.go")
	f := &fatalRecorder{}
	if got := GoSources(f, root, 3); got != nil || f.failed == "" {
		t.Errorf("two packages against a minimum of three: %v, %q", got, f.failed)
	}
	f = &fatalRecorder{}
	if GoSources(f, filepath.Join(root, "missing"), 1); f.failed == "" {
		t.Error("a missing directory didn't fail")
	}
}
