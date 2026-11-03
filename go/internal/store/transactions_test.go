package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// What each transaction is for. The connection's `_txlock=immediate` makes every BeginTx take the write
// lock unless it asks for read-only, so a read that doesn't say so queues behind writers and blocks
// readers. A new site fails TestEveryTransactionIsClassified until it is here.
var transactionClasses = map[string]string{
	"auth.InviteUser":                       "write",
	"auth.ReplaceUserRoles":                 "write",
	"auth.SetUserStatus":                    "write",
	"auth.RemoveUser":                       "write",
	"auth.IssueMagicLink":                   "write",
	"auth.ConsumeMagicLink":                 "write",
	"auth.AdoptBindingIssuers":              "write",
	"auth.AdoptBindingIssuer":               "write",
	"auth.ApplyPolicy":                      "write",
	"auth.ClearPolicy":                      "write",
	"auth.IssueLocalLink":                   "write",
	"auth.evaluatePolicy":                   "both: ResolvePolicy reads first, and writes only to claim an invitation",
	"usage.write":                           "write",
	"store.migrate":                         "write",
	"store.SaveConfigMarking":               "write",
	"store.MarkRestartPendingUnlessChanged": "write",
	"store.AcknowledgeStatic":               "write",
}

func TestEveryTransactionIsClassified(t *testing.T) {
	seen := map[string]bool{}
	fset := token.NewFileSet()
	var roots = []string{"..", "../../cmd", "../../tools"}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || (sel.Sel.Name != "BeginTx" && sel.Sel.Name != "Begin") {
						return true
					}
					name := file.Name.Name + "." + fn.Name.Name
					seen[name] = true
					if transactionClasses[name] == "" {
						t.Errorf("%s: %s begins a transaction and isn't in transactionClasses: a read should ask for read-only", fset.Position(call.Pos()), name)
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for name := range transactionClasses {
		if !seen[name] {
			t.Errorf("transactionClasses names %s, which begins no transaction", name)
		}
	}
}
