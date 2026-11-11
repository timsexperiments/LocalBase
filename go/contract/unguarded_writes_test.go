package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/testutil"
)

// Floors for TestNoUnguardedWrites: what the globs read now (15 test files here, 47 in
// go/test/system and 6 harness files at step 7's R3). Fewer means a glob missed files it guards.
const (
	minGuardedOwnFiles     = 15
	minGuardedSystemFiles  = 47
	minGuardedHarnessFiles = 6
)

// TestNoUnguardedWrites keeps every write in this package behind the guard:
// only go/test/harness/fsguard may call the os write functions directly. It reads this
// package's tests, go/test/system's (which left this package in step 7's R3) and the harness's
// files (go/test/harness, fsguard aside), each with a floor.
func TestNoUnguardedWrites(t *testing.T) {
	files, _ := filepath.Glob("*_test.go")
	root := testutil.RepoRoot(t)
	systemFiles, _ := filepath.Glob(filepath.Join(root, "go", "test", "system", "*_test.go"))
	harnessFiles, _ := filepath.Glob(filepath.Join(root, "go", "test", "harness", "*.go"))
	if len(files) < minGuardedOwnFiles || len(systemFiles) < minGuardedSystemFiles || len(harnessFiles) < minGuardedHarnessFiles {
		t.Fatalf("read %d test files here, %d in go/test/system and %d harness files, under the floors (%d, %d, %d)",
			len(files), len(systemFiles), len(harnessFiles), minGuardedOwnFiles, minGuardedSystemFiles, minGuardedHarnessFiles)
	}
	forbidden := []string{"os.WriteFile(", "os.Create(", "os.OpenFile(", "os.Symlink(", "os.Link(", ".Truncate(",
		"os.MkdirAll(", "os.Mkdir(", "os.Remove(", "os.RemoveAll(", "os.Rename(", "os.Chmod(", `"cp"`, "t.TempDir()"}
	for _, f := range append(append(files, systemFiles...), harnessFiles...) {
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
	t.Logf("read %d test files here, %d in go/test/system and %d harness files", len(files), len(systemFiles), len(harnessFiles))
}
