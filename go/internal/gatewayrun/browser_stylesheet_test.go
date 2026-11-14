package gatewayrun

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
)

// serve gives the Browser the app's style sheet where it builds it, on the very next statement and
// nowhere else, so auth's sign-in pages link the same versioned app.css as the UI's own. Read from
// run.go, like TestServeSetsTheGatewayFieldsItDid: serve has no smaller piece a test could build.
func TestServeGivesTheBrowserTheAppsStylesheet(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "run.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	built, next, sets := "", "", 0
	ast.Inspect(f, func(n ast.Node) bool {
		if block, ok := n.(*ast.BlockStmt); ok {
			for i, st := range block.List {
				as, ok := st.(*ast.AssignStmt)
				if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 || types.ExprString(as.Rhs[0]) == "" {
					continue
				}
				if call, ok := as.Rhs[0].(*ast.CallExpr); ok && types.ExprString(call.Fun) == "auth.NewBrowser" {
					built = types.ExprString(as.Lhs[0])
					if i+1 < len(block.List) {
						if nx, ok := block.List[i+1].(*ast.AssignStmt); ok && len(nx.Lhs) == 1 && len(nx.Rhs) == 1 {
							next = types.ExprString(nx.Lhs[0]) + " = " + types.ExprString(nx.Rhs[0])
						}
					}
				}
			}
		}
		if as, ok := n.(*ast.AssignStmt); ok {
			for _, l := range as.Lhs {
				if sel, ok := l.(*ast.SelectorExpr); ok && sel.Sel.Name == "Stylesheet" {
					sets++
				}
			}
		}
		return true
	})
	if built == "" {
		t.Fatal("run.go builds no auth.NewBrowser: update this test")
	}
	if want := built + ".Stylesheet = web.StylesheetURL()"; next != want || sets != 1 {
		t.Errorf("after %s := auth.NewBrowser(...), run.go does %q and sets a Stylesheet %d times; want %q, once", built, next, sets, want)
	}
}
