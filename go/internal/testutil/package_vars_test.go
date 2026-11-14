package testutil

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The guard of WAVE2 step 6: shipped code keeps no test seams in package state. Over every package
// the binary links (go list -deps ./cmd/localbase, this module's, third_party left out by path):
//   - a package-level var assigned anywhere but its declaration (any file of the module, tests
//     included, or through pkg.Var from another package), or a var of func type, is a seam unless
//     allowed below with its reason;
//   - a non-test file importing "testing" is allowed only where listed.
//
// One kind is allowed by rule: a var declared as sync.OnceValue(...), sync.OnceValues(...) or
// sync.OnceFunc(...) and never assigned elsewhere is a computed-once value, not a seam. A later
// assignment to one still fails.
//
// A seam belongs on a struct the code already receives (an unexported "(tests)" field, or an
// options value whose zero is production) or in a build-tagged file. An allowed entry that no
// longer matches anything fails too, so the list can't go stale.
var allowedPackageVars = map[string]string{
	"internal/memory.labHost": "a func-typed var set only by the localbase_lab-tagged file (host_lab.go): the build-tag pattern",
	"internal/gateway.index":  "assigned only inside indexOnce.Do; can't be a package initializer: init cycle through the route table",
	"internal/otlp.schemas":   "assigned only in init()",
}

var allowedTestingImporters = map[string]string{
	"internal/service": "testing.Testing keeps a test binary off the installed service (refuseDefaultRootInTest)",
}

const modulePath = "github.com/timsexperiments/LocalBase/go"

func TestShippedCodeKeepsNoTestSeamsInPackageState(t *testing.T) {
	moduleDir := filepath.Join(RepoRoot(t), "go")
	shipped := shippedPackages(t, moduleDir)
	if len(shipped) < 40 { // a guard that reads nothing checks nothing
		t.Fatalf("only %d shipped packages found", len(shipped))
	}
	g := newGuard(moduleDir)
	vars := map[string]*packageVar{} // "rel.Name"
	testingImporters := map[string]bool{}
	for path, dir := range shipped {
		rel := strings.TrimPrefix(path, modulePath+"/")
		files := g.parseDir(dir, false)
		funcs := topFuncs(files)
		for _, f := range files {
			imports := importsOf(f)
			for _, p := range imports {
				if p == "testing" {
					testingImporters[rel] = true
				}
			}
			for _, d := range f.Decls {
				gd, ok := d.(*ast.GenDecl)
				if !ok || gd.Tok != token.VAR {
					continue
				}
				for _, spec := range gd.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, n := range vs.Names {
						if n.Name == "_" {
							continue
						}
						v := &packageVar{rel: rel, name: n.Name, at: g.fset.Position(n.Pos()).String()}
						if _, ok := vs.Type.(*ast.FuncType); ok {
							v.why = append(v.why, "of func type")
						} else if i < len(vs.Values) {
							if isOnce(vs.Values[i], imports) {
								v.once = true
							} else if why := g.funcValue(vs.Values[i], funcs, imports); why != "" {
								v.why = append(v.why, why)
							}
						}
						vars[rel+"."+n.Name] = v
					}
				}
			}
		}
	}
	g.findAssignments(t, moduleDir, shipped, vars)

	var found []string
	for key, v := range vars {
		if len(v.why) == 0 {
			continue
		}
		if _, ok := allowedPackageVars[key]; ok {
			v.allowed = true
			continue
		}
		found = append(found, key+" ("+strings.Join(v.why, "; ")+") at "+v.at)
	}
	for key := range allowedPackageVars {
		if v, ok := vars[key]; !ok || !v.allowed {
			found = append(found, key+": allowed, but nothing here matches it any more; remove it")
		}
	}
	for rel := range testingImporters {
		if _, ok := allowedTestingImporters[rel]; !ok {
			found = append(found, rel+` imports "testing" in a non-test file`)
		}
	}
	for rel := range allowedTestingImporters {
		if !testingImporters[rel] {
			found = append(found, rel+`: allowed to import "testing", but no longer does; remove it`)
		}
	}
	sort.Strings(found)
	for _, f := range found {
		t.Error(f)
	}
}

type packageVar struct {
	rel, name, at string
	why           []string
	allowed       bool
	once          bool // declared as sync.OnceValue/OnceValues/OnceFunc: allowed by rule unless assigned
}

// isOnce says whether a var's value is sync.OnceValue(...), sync.OnceValues(...) or sync.OnceFunc(...).
func isOnce(e ast.Expr, imports map[string]string) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	var fun ast.Expr = call.Fun
	if ix, ok := fun.(*ast.IndexExpr); ok { // sync.OnceValue[T](...)
		fun = ix.X
	} else if ix, ok := fun.(*ast.IndexListExpr); ok {
		fun = ix.X
	}
	s, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := s.X.(*ast.Ident)
	return ok && imports[x.Name] == "sync" && (s.Sel.Name == "OnceValue" || s.Sel.Name == "OnceValues" || s.Sel.Name == "OnceFunc")
}

// shippedPackages is this module's packages the binary links, by import path, with their dirs.
func shippedPackages(t *testing.T, moduleDir string) map[string]string {
	t.Helper()
	cmd := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}} {{.Dir}}{{end}}", "./cmd/localbase")
	cmd.Dir = moduleDir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	pkgs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		path, dir, ok := strings.Cut(line, " ")
		if ok && strings.HasPrefix(path, modulePath+"/") && !strings.Contains(path, "/third_party/") {
			pkgs[path] = dir
		}
	}
	return pkgs
}

type guard struct {
	moduleDir string
	fset      *token.FileSet
	parsed    map[string][]*ast.File // dir+tests → files
	topFuncs  map[string]map[string]bool
}

func newGuard(moduleDir string) *guard {
	return &guard{moduleDir: moduleDir, fset: token.NewFileSet(), parsed: map[string][]*ast.File{}, topFuncs: map[string]map[string]bool{}}
}

// parseDir parses a directory's .go files, all build tags alike, tests only if asked.
func (g *guard) parseDir(dir string, tests bool) []*ast.File {
	key := dir + "|" + strconv.FormatBool(tests)
	if fs, ok := g.parsed[key]; ok {
		return fs
	}
	entries, _ := os.ReadDir(dir)
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || (!tests && strings.HasSuffix(name, "_test.go")) {
			continue
		}
		f, err := parser.ParseFile(g.fset, filepath.Join(dir, name), nil, 0)
		if err == nil {
			files = append(files, f)
		}
	}
	g.parsed[key] = files
	return files
}

func topFuncs(files []*ast.File) map[string]bool {
	funcs := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil {
				funcs[fd.Name.Name] = true
			}
		}
	}
	return funcs
}

// importsOf maps each import's name in the file to its path.
func importsOf(f *ast.File) map[string]string {
	m := map[string]string{}
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		name := path[strings.LastIndex(path, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		m[name] = path
	}
	return m
}

// funcValue says why a var's initial value makes it a func, or "".
func (g *guard) funcValue(e ast.Expr, funcs map[string]bool, imports map[string]string) string {
	switch v := e.(type) {
	case *ast.FuncLit:
		return "a func literal"
	case *ast.Ident:
		if funcs[v.Name] {
			return "the func " + v.Name
		}
	case *ast.SelectorExpr:
		if x, ok := v.X.(*ast.Ident); ok {
			if path, ok := imports[x.Name]; ok && g.isTopFunc(path, v.Sel.Name) {
				return "the func " + path + "." + v.Sel.Name
			}
		}
	}
	return ""
}

// isTopFunc says whether an import path's package declares name as a top-level func.
func (g *guard) isTopFunc(path, name string) bool {
	funcs, ok := g.topFuncs[path]
	if !ok {
		dir := filepath.Join(goruntime.GOROOT(), "src", path)
		if strings.HasPrefix(path, modulePath+"/") {
			dir = filepath.Join(g.moduleDir, strings.TrimPrefix(path, modulePath+"/"))
		}
		funcs = topFuncs(g.parseDir(dir, false))
		g.topFuncs[path] = funcs
	}
	return funcs[name]
}

// findAssignments marks every var assigned outside its declaration, from any file of the module:
// in its own package (tests too), or as pkg.Var from another.
func (g *guard) findAssignments(t *testing.T, moduleDir string, shipped map[string]string, vars map[string]*packageVar) {
	t.Helper()
	relOfDir := map[string]string{}
	for path, dir := range shipped {
		relOfDir[dir] = strings.TrimPrefix(path, modulePath+"/")
	}
	err := filepath.WalkDir(moduleDir, func(dir string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		if strings.Contains(dir, string(filepath.Separator)+"third_party") || strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" {
			return filepath.SkipDir
		}
		own := relOfDir[dir]
		for _, f := range g.parseDir(dir, true) {
			imports := importsOf(f)
			mark := func(e ast.Expr, how string) {
				var key string
				switch x := e.(type) {
				case *ast.Ident:
					// Only an identifier this file doesn't declare locally: a package-level var.
					if own == "" || (x.Obj != nil && !isPackageVarDecl(f, x.Obj)) {
						return
					}
					key = own + "." + x.Name
				case *ast.SelectorExpr:
					pkg, ok := x.X.(*ast.Ident)
					if !ok || pkg.Obj != nil {
						return
					}
					path, ok := imports[pkg.Name]
					if !ok || !strings.HasPrefix(path, modulePath+"/") {
						return
					}
					key = strings.TrimPrefix(path, modulePath+"/") + "." + x.Sel.Name
				default:
					return
				}
				if v, ok := vars[key]; ok {
					at := g.fset.Position(e.Pos())
					v.why = append(v.why, how+" at "+filepath.Base(at.Filename)+":"+strconv.Itoa(at.Line))
				}
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch s := n.(type) {
				case *ast.AssignStmt:
					if s.Tok != token.DEFINE {
						for _, l := range s.Lhs {
							mark(l, "assigned")
						}
					}
				case *ast.IncDecStmt:
					mark(s.X, "changed")
				case *ast.UnaryExpr:
					if s.Op == token.AND {
						mark(s.X, "its address taken")
					}
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

// isPackageVarDecl says whether obj is a package-level var declared in f.
func isPackageVarDecl(f *ast.File, obj *ast.Object) bool {
	vs, ok := obj.Decl.(*ast.ValueSpec)
	if !ok || obj.Kind != ast.Var {
		return false
	}
	for _, d := range f.Decls {
		if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.VAR {
			for _, s := range gd.Specs {
				if s == vs {
					return true
				}
			}
		}
	}
	return false
}
