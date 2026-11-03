package gateway

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// errorCodeShape is what a code looks like: `model_not_found`. It leaves out the hyphenated codes
// auth uses inside this program, which manage.go maps to these before anyone sees them.
var errorCodeShape = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)

// notErrorCodePackages hold no code a request can get back, so a package added later is scanned
// unless someone says otherwise here.
var notErrorCodePackages = map[string]string{
	"cli":           "Its codes describe a command's result",
	"web":           "It reads codes, for what its pages say",
	"third_party":   "Vendored",
	"fakebackend":   "A stand-in for a backend, for tests",
	"testutil":      "For tests",
	"placementtest": "For tests",
}

const videoClientOnly = "video.ClientError's. Nothing outside the video client reads them, and a job reports video_generation_failed"

// notErrorCodes passes the scan's rules, but is no code a request can get back. Each says why.
var notErrorCodes = map[string]string{
	"backend_media_too_large":    videoClientOnly,
	"backend_request_failed":     videoClientOnly,
	"backend_response_invalid":   videoClientOnly,
	"backend_response_too_large": videoClientOnly,
	"invalid_base_url":           videoClientOnly,
	"invalid_job_id":             videoClientOnly,
}

// notDocumentedCodes sit beside a status in the design notes, and aren't this gateway's codes.
var notDocumentedCodes = map[string]string{
	"gateway_error":         "What the TS build answered, in a note comparing the two",
	"in_flight":             "A field of a 409's body",
	"invalid_request_error": "An error.type",
	"server_error":          "An error.type, and what a backend said",
	"memory_pressure":       "A reason in the logs, which the page says beside the 503 it explains",
}

// codesArriving are named in this branch's notes, and sent by a PR that isn't under it yet. Each
// entry fails once its code is here, so the table can't go stale.
var codesArriving = map[string]string{
	"remote_media_not_allowed": "#529",
	"request_timeout":          "#482",
}

// sourceErrorCodes reads the code out of the source, so that a new one can't be missed by a test
// that has to think of asking for it. It reads each way this code base hands over a code:
//
//	writeError(w, r, 400, "t", "code", ...)      any function with a parameter named code
//	Error{Code: "code"}, map[string]string{"code": "code"}
//	mgmtError{"code", "message"}                 positional, by the struct's field name
//	code := "code"
//	case "code":                                 in a switch on something's Code
//	e.Code == "code"
//	{"code", 409}                                a code and its status, in a table
//	managementMessages                           the table of management's own codes
//	const CodeNoSpace = "code"                   a constant named for what it is
//	`{"error":{"code":"code"}}`                  a body written out by hand
func sourceErrorCodes(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	var files []*ast.File
	err := filepath.WalkDir("..", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if _, skip := notErrorCodePackages[d.Name()]; skip {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return err
		}
		files = append(files, f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	isCodeName := func(s string) bool { return s == "code" || s == "Code" || s == "errorCode" || s == "ErrorCode" }
	text := func(e ast.Expr) (string, bool) {
		b, ok := e.(*ast.BasicLit)
		if !ok || b.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(b.Value)
		return s, err == nil
	}
	named := func(e ast.Expr) bool {
		switch x := e.(type) {
		case *ast.Ident:
			return isCodeName(x.Name)
		case *ast.SelectorExpr:
			return isCodeName(x.Sel.Name)
		}
		return false
	}
	// Which argument of which function is a code, by the parameter's name, and which field of which
	// struct, by the field's.
	codeArg, codeField := map[string]int{}, map[string]int{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			spec, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			if st, ok := spec.Type.(*ast.StructType); ok {
				i := 0
				for _, field := range st.Fields.List {
					for _, name := range field.Names {
						if isCodeName(name.Name) {
							codeField[spec.Name.Name] = i
						}
						i++
					}
				}
			}
			return true
		})
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			i := 0
			for _, field := range fn.Type.Params.List {
				for _, name := range field.Names {
					if isCodeName(name.Name) {
						codeArg[fn.Name.Name] = i
					}
					i++
				}
			}
		}
	}
	found := map[string]string{}
	note := func(code string, pos token.Pos) {
		if _, seen := found[code]; !seen && errorCodeShape.MatchString(code) {
			p := fset.Position(pos)
			found[code] = strings.TrimPrefix(filepath.ToSlash(p.Filename), "../") + ":" + strconv.Itoa(p.Line)
		}
	}
	add := func(e ast.Expr) {
		if s, ok := text(e); ok {
			note(s, e.Pos())
		}
	}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BasicLit:
				if s, ok := text(x); ok {
					for _, m := range handWrittenCode.FindAllStringSubmatch(s, -1) {
						note(m[1], x.Pos())
					}
				}
			case *ast.CallExpr:
				name := ""
				switch fn := x.Fun.(type) {
				case *ast.Ident:
					name = fn.Name
				case *ast.SelectorExpr:
					name = fn.Sel.Name
				}
				if i, ok := codeArg[name]; ok && i < len(x.Args) {
					add(x.Args[i])
				}
			case *ast.KeyValueExpr:
				if named(x.Key) {
					add(x.Value)
				}
				if k, ok := text(x.Key); ok && isCodeName(k) {
					add(x.Value)
				}
			case *ast.AssignStmt:
				for i, l := range x.Lhs {
					if named(l) && i < len(x.Rhs) {
						add(x.Rhs[i])
					}
				}
			case *ast.BinaryExpr:
				if x.Op == token.EQL || x.Op == token.NEQ {
					if named(x.X) {
						add(x.Y)
					}
					if named(x.Y) {
						add(x.X)
					}
				}
			case *ast.SwitchStmt:
				if x.Tag != nil && named(x.Tag) {
					for _, c := range x.Body.List {
						for _, e := range c.(*ast.CaseClause).List {
							add(e)
						}
					}
				}
			case *ast.CompositeLit:
				name := ""
				switch ty := x.Type.(type) {
				case *ast.Ident:
					name = ty.Name
				case *ast.SelectorExpr:
					name = ty.Sel.Name
				}
				if i, ok := codeField[name]; ok && i < len(x.Elts) {
					if _, keyed := x.Elts[i].(*ast.KeyValueExpr); !keyed {
						add(x.Elts[i])
					}
				}
				if len(x.Elts) == 2 {
					if st, ok := x.Elts[1].(*ast.BasicLit); ok && st.Kind == token.INT {
						if n, err := strconv.Atoi(st.Value); err == nil && n >= 400 && n < 600 {
							add(x.Elts[0])
						}
					}
				}
			case *ast.ValueSpec:
				for i, name := range x.Names {
					if (isCodeName(name.Name) || strings.HasPrefix(name.Name, "Code")) && i < len(x.Values) {
						add(x.Values[i])
					}
					if name.Name == "managementMessages" && i < len(x.Values) {
						if table, ok := x.Values[i].(*ast.CompositeLit); ok {
							for _, e := range table.Elts {
								add(e.(*ast.KeyValueExpr).Key)
							}
						}
					}
				}
			}
			return true
		})
	}
	return found
}

// handWrittenCode is the code in a JSON body written out as a string.
var handWrittenCode = regexp.MustCompile(`"code":\s*"([a-z][a-z0-9_]*)"`)

// documentedStatusCode finds a code said beside its status: `400 some_code`, 400 `some_code`,
// `400` with `some_code`.
var documentedStatusCode = regexp.MustCompile("\\b[45][0-9][0-9]`?(?: with| and)? `?([a-z][a-z0-9]*(?:_[a-z0-9]+)+)\\b")

// documentedErrorCodes reads the code out of the design notes and the pages.
func documentedErrorCodes(t *testing.T) map[string]string {
	t.Helper()
	paths := []string{filepath.Join("..", "..", "DESIGN.md")}
	pages, err := filepath.Glob(filepath.Join("..", "..", "..", "docs", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, p := range append(paths, pages...) {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			for _, m := range documentedStatusCode.FindAllStringSubmatch(line, -1) {
				if _, seen := found[m[1]]; !seen {
					found[m[1]] = filepath.Base(p) + ":" + strconv.Itoa(i+1)
				}
			}
		}
	}
	return found
}

func errorCodeEnum(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas struct {
				ErrorCode struct {
					Enum []string `json:"enum"`
				} `json:"ErrorCode"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Components.Schemas.ErrorCode.Enum) == 0 {
		t.Fatal("openapi.json has no ErrorCode list")
	}
	return doc.Components.Schemas.ErrorCode.Enum
}

const errorCodeFix = "Fix: the list is components.schemas.ErrorCode in internal/gateway/openapi.json. Then from go/: go run ./tools/openapi-docs"

// A code is an API: clients branch on it. So every code the source can send and every code the
// notes and pages name is on the document's list, and everything on that list is really sent.
func TestOpenAPIListsEveryErrorCode(t *testing.T) {
	enum := errorCodeEnum(t)
	if !slices.IsSorted(enum) {
		t.Error("ErrorCode's list is not sorted, so its diffs and the page would churn")
	}
	source := sourceErrorCodes(t)
	if len(source) < 50 {
		t.Fatalf("the scan found %d codes, so it has stopped reading the source", len(source))
	}
	for _, code := range sortedStrings(source) {
		_, excluded := notErrorCodes[code]
		switch listed := slices.Contains(enum, code); {
		case excluded && listed:
			t.Errorf("%s is on the list and in notErrorCodes (%s)", code, notErrorCodes[code])
		case !excluded && !listed:
			t.Errorf("%s is sent (%s) and isn't on the list. %s", code, source[code], errorCodeFix)
		}
	}
	for code := range notErrorCodes {
		if _, ok := source[code]; !ok {
			t.Errorf("notErrorCodes has %s, which the source no longer has", code)
		}
	}
	for _, code := range enum {
		if _, ok := source[code]; !ok {
			t.Errorf("%s is on the list, and the scan can't find it in the source. %s", code, errorCodeFix)
		}
	}
	documented := documentedErrorCodes(t)
	for _, code := range sortedStrings(documented) {
		_, notACode := notDocumentedCodes[code]
		_, arriving := codesArriving[code]
		if !notACode && !arriving && !slices.Contains(enum, code) {
			t.Errorf("%s is named as an answer (%s) and isn't on the list. If it's sent, add it. If not, fix that line. %s", code, documented[code], errorCodeFix)
		}
	}
	for code, pr := range codesArriving {
		if _, here := source[code]; here {
			t.Errorf("%s has arrived, with %s. Remove it from codesArriving, and add it to the list. %s", code, pr, errorCodeFix)
		}
	}
}

// The list and a route's own words have to agree: a code a route mentions is on the list.
func TestOpenAPIRoutesNameOnlyListedCodes(t *testing.T) {
	enum := errorCodeEnum(t)
	raw, err := os.ReadFile("openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	backticked := regexp.MustCompile("`([a-z][a-z0-9]*(?:_[a-z0-9]+)+)`")
	var walk func(where string, v any)
	walk = func(where string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, inner := range x {
				if k == "description" {
					if s, ok := inner.(string); ok && (strings.Contains(where, "/responses/") || strings.HasPrefix(where, "responses/")) {
						for _, m := range backticked.FindAllStringSubmatch(s, -1) {
							if !slices.Contains(enum, m[1]) {
								t.Errorf("%s names %s, which isn't on the list. %s", where, m[1], errorCodeFix)
							}
						}
					}
				}
				walk(where+"/"+k, inner)
			}
		case []any:
			for _, inner := range x {
				walk(where, inner)
			}
		}
	}
	walk("", doc["paths"])
	walk("responses", doc["components"].(map[string]any)["responses"])
}

func sortedStrings[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
