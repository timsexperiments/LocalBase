package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"testing"
)

// serveFields is every gateway.Server field serve sets: in its literal and by assignment before
// gateway.New, and by assignment after it. When the gateway takes a Config (wave 2, 4d), serve
// sets these through it; this list moves with that change, so a field left out is a failure here
// and not a gateway that quietly runs without it.
var serveFields = struct{ before, after []string }{
	before: []string{"Auth", "Browser", "Catalog", "Image", "LLM", "Library", "Log", "LogOutages", "LoginPages", "Memory",
		"MemoryGB", "NoAuth", "Otel", "Overrides", "Placement", "Root", "RunMemoryLimit", "RunsMemoryLimit", "STT", "Store",
		"TTS", "UI", "Updates", "Usage", "Video", "VideoJobs", "Voices"},
	after: []string{"OnReady"},
}

func TestServeSetsTheGatewayFieldsItDid(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "serve.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	srv := ""           // the variable bound to &gateway.Server{...}
	var newAt token.Pos // where gateway.New(srv) is called
	var before, after []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if len(n.Rhs) == 1 && srv == "" {
				if u, ok := n.Rhs[0].(*ast.UnaryExpr); ok {
					if lit, ok := u.X.(*ast.CompositeLit); ok && isGatewayServer(lit.Type) {
						srv = n.Lhs[0].(*ast.Ident).Name
						for _, e := range lit.Elts {
							before = append(before, e.(*ast.KeyValueExpr).Key.(*ast.Ident).Name)
						}
						return true
					}
				}
			}
			for _, l := range n.Lhs {
				if sel, ok := l.(*ast.SelectorExpr); ok && srv != "" {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == srv {
						if newAt.IsValid() && n.Pos() > newAt {
							after = append(after, sel.Sel.Name)
						} else {
							before = append(before, sel.Sel.Name)
						}
					}
				}
			}
		case *ast.CallExpr:
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "New" && isPkg(sel.X, "gateway") {
				newAt = n.Pos()
			}
		}
		return true
	})
	if srv == "" || !newAt.IsValid() {
		t.Fatal("serve.go no longer builds a &gateway.Server and calls gateway.New: update this test with it")
	}
	slices.Sort(before)
	slices.Sort(after)
	if !slices.Equal(before, serveFields.before) {
		t.Errorf("set before gateway.New:\n got %q\nwant %q", before, serveFields.before)
	}
	if !slices.Equal(after, serveFields.after) {
		t.Errorf("set after gateway.New:\n got %q\nwant %q", after, serveFields.after)
	}
}

func isGatewayServer(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Server" && isPkg(sel.X, "gateway")
}

func isPkg(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}
