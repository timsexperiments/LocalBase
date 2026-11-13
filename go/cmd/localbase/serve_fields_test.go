package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"testing"
)

// serveFields is every gateway.Config field serve sets: in its literal and by assignment before
// gateway.New, and by assignment after it. It was the Server's fields until the gateway took a
// Config (wave 2, 4d-2b), and moved with that change, so a field left out is a failure here and
// not a gateway that quietly runs without it. Nothing is set after New: OnReady is now set before.
var serveFields = struct{ before, after []string }{
	before: []string{"Auth", "Browser", "Catalog", "Image", "LLM", "Library", "Log", "LogOutages", "LoginPages", "Memory",
		"MemoryGB", "NoAuth", "OnReady", "Otel", "Overrides", "Placement", "Root", "RunMemoryLimit", "RunsMemoryLimit", "STT", "Store",
		"TTS", "UI", "Updates", "Usage", "Video", "VideoJobs", "Voices"},
	after: nil,
}

func TestServeSetsTheGatewayFieldsItDid(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "run.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	gw := ""            // the variable bound to gateway.Config{...}
	var newAt token.Pos // where gateway.New(gw) is called
	var before, after []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if len(n.Rhs) == 1 && gw == "" {
				if lit, ok := n.Rhs[0].(*ast.CompositeLit); ok && isGatewayConfig(lit.Type) {
					gw = n.Lhs[0].(*ast.Ident).Name
					for _, e := range lit.Elts {
						before = append(before, e.(*ast.KeyValueExpr).Key.(*ast.Ident).Name)
					}
					return true
				}
			}
			for _, l := range n.Lhs {
				if sel, ok := l.(*ast.SelectorExpr); ok && gw != "" {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == gw {
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
	if gw == "" || !newAt.IsValid() {
		t.Fatal("run.go no longer builds a gateway.Config and calls gateway.New: update this test with it")
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

func isGatewayConfig(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Config" && isPkg(sel.X, "gateway")
}

func isPkg(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

// serve closes the gateway after its ordered stop, as Close's doc asks: the runs and management are
// stopped first, each in its own way, and Close comes after both.
func TestServeClosesTheGatewayLast(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "run.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	at := map[string]token.Pos{}
	ast.Inspect(f, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if sel, ok := c.Fun.(*ast.SelectorExpr); ok && isPkg(sel.X, "srv") {
				if _, seen := at[sel.Sel.Name]; !seen || sel.Sel.Name == "Close" {
					at[sel.Sel.Name] = c.Pos() // the first of each, the last Close
				}
			}
		}
		return true
	})
	closeAt, ok := at["Close"]
	if !ok {
		t.Fatal("serve never closes the gateway")
	}
	for _, before := range []string{"StopRuns", "StopManagement"} {
		if p, ok := at[before]; !ok || p > closeAt {
			t.Errorf("serve closes the gateway before srv.%s", before)
		}
	}
}
