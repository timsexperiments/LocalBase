package gatewaytest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const importPath = "github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"

// This package is for tests: no code built into a binary imports it, whatever its build tags.
func TestOnlyTestsImportThis(t *testing.T) {
	tests := 0
	err := filepath.WalkDir("../../..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "testdata" || name == "node_modules" || (strings.HasPrefix(name, ".") && path != "../../..") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == importPath {
				if strings.HasSuffix(path, "_test.go") {
					tests++
				} else {
					t.Errorf("%s imports gatewaytest, and only tests may", path)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The check means nothing if it isn't reading the tree the tests are in.
	if tests == 0 {
		t.Error("no test imports gatewaytest: is the walk reading the module?")
	}
}
