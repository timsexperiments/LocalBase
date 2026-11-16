package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A process loses its locks on the database when it closes any descriptor on that file, whatever opened
// it, and the gateway's lock is what tells another process it's attached (internal/store/held_file.go).
// internal/backup opens and closes the files it hashes, the database among them. So nothing the gateway
// runs may reach it. This follows every function serve refers to in this package, by name, called or
// passed, which errs towards finding too much, and fails on any use of internal/backup among them.
func TestServeNeverReachesInternalBackup(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	funcs := map[string]*ast.FuncDecl{}
	methods := map[string][]*ast.FuncDecl{} // by name, whatever the receiver: a method called is followed
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range parsed.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
				funcs[fn.Name.Name] = fn
			} else if ok {
				methods[fn.Name.Name] = append(methods[fn.Name.Name], fn)
			}
		}
	}
	if funcs["serve"] == nil {
		t.Fatal("no serve function to start from")
	}
	type item struct {
		name string
		decl *ast.FuncDecl
	}
	reached, queue := map[*ast.FuncDecl]bool{funcs["serve"]: true}, []item{{"serve", funcs["serve"]}}
	follow := func(name string, decl *ast.FuncDecl) {
		if !reached[decl] {
			reached[decl] = true
			queue = append(queue, item{name, decl})
		}
	}
	var uses []string
	for len(queue) > 0 {
		at := queue[0]
		queue = queue[1:]
		ast.Inspect(at.decl, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if pkg, ok := x.X.(*ast.Ident); ok && pkg.Name == "backup" {
					uses = append(uses, at.name+" uses backup."+x.Sel.Name+" at "+fset.Position(x.Pos()).String())
				}
				for _, m := range methods[x.Sel.Name] {
					follow(x.Sel.Name, m)
				}
			case *ast.Ident:
				if fn := funcs[x.Name]; fn != nil {
					follow(x.Name, fn)
				}
			}
			return true
		})
	}
	sort.Strings(uses)
	for _, use := range uses {
		t.Errorf("reachable from serve: %s", use)
	}
	if len(reached) < 5 {
		t.Errorf("serve reached only %d functions; the walk isn't following anything", len(reached))
	}
}
