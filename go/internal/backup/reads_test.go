package backup

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// failedReadExceptions may answer "absent" when a read fails. Each says why that is safe. An entry
// for a function this package has must be needed, so that the list can't go stale.
var failedReadExceptions = map[string]string{
	"compareWithTarget": "It says so in the report's cautions. A root is often replaced because its database is broken, so this can't refuse",
	"isDir":             "Whether --output names a directory. On a failure it goes on to create the file, and that reports the problem",
	"exists":            "A stat of a path this command owns. A failure that isn't \"missing\" shows at the rename that follows",
	"abandoned":         "Leftovers to mention or sweep. A directory it can't read has none it may remove",
	"liveSignIn":        "A failure travels in its result, and the report then says that root's value can't be read, so it never reads as having none",
	"UnfinishedWrite":   "It only answers whether a crash left this database mid-write, and any other failure means it didn't. #531 adds it",
}

// readCalls are what reach a database or a file for data.
var readCalls = []string{"Scan", "QueryRowContext", "QueryContext", "QueryRow", "Query", "ExecContext", "Exec", "openReadOnly", "OpenReadOnly", "Open", "LoadConfig", "ReadFile", "ReadDir", "readableDatabase"}

// TestNoFailedReadLooksLikeAbsent is the third time this bug would have been found by accident.
// `hasTable` answered "no" on any error, and a busy moment then read as no migrations, no models, no
// settings and no revocations. So a read here has three ways to go wrong, and none is allowed:
//
//	return err == nil && n > 0             the answer swallows the failure, or just err == nil
//	if err != nil { return nil, nil }      a failure returned as success
//	func f(...) bool { ...read... }        no way to report a failure at all
//
// Its limits: it reads this package only, and it reads patterns. A failure hidden any other way gets
// past it. selectedModels did, in a closure that returned nil, nil, and was found by reading.
func TestNoFailedReadLooksLikeAbsent(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	used, defined := map[string]bool{}, map[string]bool{}
	checked := 0
	report := func(fn string, pos token.Pos, what string) {
		if _, ok := failedReadExceptions[fn]; ok {
			used[fn] = true
			return
		}
		p := fset.Position(pos)
		t.Errorf("%s:%d: %s %s. Return the error, or add it to failedReadExceptions saying why it is safe", p.Filename, p.Line, fn, what)
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			checked++
			defined[fn.Name.Name] = true
			checkFailedReads(fn.Name.Name, fn.Type, fn.Body, report)
		}
	}
	if checked < 30 {
		t.Fatalf("read %d functions, so this has stopped reading the package", checked)
	}
	for fn, why := range failedReadExceptions {
		if defined[fn] && !used[fn] {
			t.Errorf("failedReadExceptions has %s (%s), which no longer needs it", fn, why)
		}
	}
}

func returnsError(ty *ast.FuncType) bool {
	if ty.Results == nil {
		return false
	}
	for _, r := range ty.Results.List {
		if id, ok := r.Type.(*ast.Ident); ok && id.Name == "error" {
			return true
		}
	}
	return false
}

func isErrIdent(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "err"
}

func checkFailedReads(name string, ty *ast.FuncType, body *ast.BlockStmt, report func(fn string, pos token.Pos, what string)) {
	reads := false
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			checkFailedReads(name, x.Type, x.Body, report)
			return false
		case *ast.CallExpr:
			callee := ""
			switch f := x.Fun.(type) {
			case *ast.Ident:
				callee = f.Name
			case *ast.SelectorExpr:
				callee = f.Sel.Name
			}
			if slices.Contains(readCalls, callee) {
				reads = true
			}
		case *ast.ReturnStmt:
			for _, r := range x.Results {
				b, ok := r.(*ast.BinaryExpr)
				if !ok {
					continue
				}
				if b.Op == token.LAND {
					b, ok = b.X.(*ast.BinaryExpr)
				}
				if ok && b.Op == token.EQL && isErrIdent(b.X) {
					report(name, x.Pos(), "answers with err == nil")
				}
			}
		case *ast.IfStmt:
			c, ok := x.Cond.(*ast.BinaryExpr)
			if !ok || c.Op != token.NEQ || !isErrIdent(c.X) || !returnsError(ty) {
				return true
			}
			for _, s := range x.Body.List {
				ret, ok := s.(*ast.ReturnStmt)
				if !ok || len(ret.Results) == 0 {
					continue
				}
				if last, ok := ret.Results[len(ret.Results)-1].(*ast.Ident); ok && last.Name == "nil" {
					report(name, ret.Pos(), "returns success from a failed read")
				}
			}
		}
		return true
	})
	if reads && !returnsError(ty) && ty.Results != nil && len(ty.Results.List) > 0 {
		report(name, body.Pos(), "reads, and has no way to report a failure")
	}
}
