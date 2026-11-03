package contract

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
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
	names, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
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
	return found
}

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
	found := skipsOnPlatform(t, ".")
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
	dir := scratchDir(t)
	const planted = `package p
import ("runtime"; "testing")
func TestPlanted(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("no")
	}
}
`
	if err := mustWriteFile(filepath.Join(dir, "p_test.go"), []byte(planted), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := skipsOnPlatform(t, dir); !strings.HasSuffix(got["TestPlanted"], ":4:2") {
		t.Errorf("the search missed a planted skip: %v", got)
	}
}
