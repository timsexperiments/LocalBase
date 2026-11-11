package contract

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/testutil"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// platformAbsences names each test that doesn't run on every platform: where it is absent,
// why, and which CI job does run it. A skip on the platform alone is not allowed anywhere else,
// because a test that quietly stops running looks just like one that passes.
var platformAbsences = map[string]struct{ absentOn, why, ranBy string }{
	"TestDifferentialRetrySemanticsFake": {
		absentOn: "Linux, Intel Macs",
		why:      "whisper's backends need Metal or Vulkan, so the request stops at backend_not_selected and never reaches the start under test",
		ranBy:    "go.yml test (macos-14, Apple Silicon), and go-dual.yml for the comparison with TS",
	},
}

// skipsOnPlatform lists the test functions that call t.Skip in an if that reads GOOS or GOARCH.
func skipsOnPlatform(t *testing.T, dir string) map[string]string {
	t.Helper()
	_, found := skipsIn(t, filepath.Join(dir, "*_test.go"))
	return found
}

// skipsIn is skipsOnPlatform over the files a glob names, any function in them, with the number
// of files it read.
func skipsIn(t *testing.T, pattern string) (int, map[string]string) {
	t.Helper()
	names, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	fset := token.NewFileSet()
	for _, name := range names {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				stmt, ok := n.(*ast.IfStmt)
				if !ok || !mentions(stmt.Cond, "GOOS", "GOARCH") || !mentions(stmt.Body, "Skip", "Skipf", "SkipNow") {
					return true
				}
				found[fn.Name.Name] = fset.Position(stmt.Pos()).String()
				return true
			})
		}
	}
	return len(names), found
}

// Floors for TestPlatformAbsencesAreNamed: below these, a glob found too little to vouch for
// (57 test files here, and 7 files in go/test/harness and its fsguard, at step 7's R2).
const (
	minAbsenceOwnFiles     = 50
	minAbsenceHarnessFiles = 6
)

func mentions(n ast.Node, words ...string) bool {
	seen := false
	ast.Inspect(n, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			for _, w := range words {
				seen = seen || id.Name == w
			}
		}
		return !seen
	})
	return seen
}

func TestPlatformAbsencesAreNamed(t *testing.T) {
	own, found := skipsIn(t, "*_test.go")
	// The harness left this package in step 7 (R2). Its files, tests or not, are read too, so a
	// platform skip there is still found: a move must not narrow a source-reading check.
	harness := filepath.Join(testutil.RepoRoot(t), "go", "test", "harness")
	harnessFiles := 0
	for _, dir := range []string{harness, filepath.Join(harness, "fsguard")} {
		n, more := skipsIn(t, filepath.Join(dir, "*.go"))
		harnessFiles += n
		maps.Copy(found, more)
	}
	if own < minAbsenceOwnFiles || harnessFiles < minAbsenceHarnessFiles {
		t.Fatalf("read %d test files and %d harness files, under the floors (%d, %d)",
			own, harnessFiles, minAbsenceOwnFiles, minAbsenceHarnessFiles)
	}
	for name, where := range found {
		if _, ok := platformAbsences[name]; !ok {
			t.Errorf("%s: %s skips on the platform and isn't in platformAbsences", where, name)
		}
	}
	for name, a := range platformAbsences {
		if found[name] == "" {
			t.Errorf("platformAbsences names %s, which no longer skips on the platform", name)
		}
		if a.absentOn == "" || a.why == "" || a.ranBy == "" {
			t.Errorf("%s: say where it is absent, why, and what runs it", name)
		}
	}

	// The search finds what it is for, or this says nothing.
	dir := fsguard.ScratchDir(t)
	const planted = `package p
import ("runtime"; "testing")
func TestPlanted(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("no")
	}
}
`
	if err := fsguard.WriteFile(filepath.Join(dir, "p_test.go"), []byte(planted), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := skipsOnPlatform(t, dir); !strings.HasSuffix(got["TestPlanted"], ":4:2") {
		t.Errorf("the search missed a planted skip: %v", got)
	}
}
