package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/testutil"
)

// Floors for TestNoUnguardedWrites: below these, a glob missed the files it guards (57 test
// files here and 5 harness files at step 7's R2).
const (
	minGuardedOwnFiles     = 50
	minGuardedHarnessFiles = 4
)

// TestNoUnguardedWrites keeps every write in this package behind the guard:
// only go/test/harness/fsguard may call the os write functions directly. It reads this
// package's tests and the harness's files (go/test/harness, fsguard aside), each with a floor.
func TestNoUnguardedWrites(t *testing.T) {
	files, _ := filepath.Glob("*_test.go")
	harnessFiles, _ := filepath.Glob(filepath.Join(testutil.RepoRoot(t), "go", "test", "harness", "*.go"))
	if len(files) < minGuardedOwnFiles || len(harnessFiles) < minGuardedHarnessFiles {
		t.Fatalf("read %d test files and %d harness files, under the floors (%d, %d)",
			len(files), len(harnessFiles), minGuardedOwnFiles, minGuardedHarnessFiles)
	}
	forbidden := []string{"os.WriteFile(", "os.Create(", "os.OpenFile(", "os.Symlink(", "os.Link(", ".Truncate(",
		"os.MkdirAll(", "os.Mkdir(", "os.Remove(", "os.RemoveAll(", "os.Rename(", "os.Chmod(", `"cp"`, "t.TempDir()"}
	for _, f := range append(files, harnessFiles...) {
		if f == "unguarded_writes_test.go" {
			continue // the list itself
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			code := line
			if j := strings.Index(code, "//"); j >= 0 {
				code = code[:j]
			}
			for _, call := range forbidden {
				if strings.Contains(code, call) {
					t.Errorf("%s:%d: %s outside the guard: %s", f, i+1, strings.TrimSuffix(call, "("), strings.TrimSpace(line))
				}
			}
		}
	}
	t.Logf("read %d test files and %d harness files", len(files), len(harnessFiles))
}
