package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// printsDirectly is everywhere that writes to a terminal without Ctx.Info, Ctx.Error or Ctx.Out, and
// why that's safe. A call whose arguments are all literals needs no entry.
var printsDirectly = map[string]string{
	"internal/cli/cli.go:Info":                 "the escaping itself",
	"internal/cli/cli.go:Error":                "the escaping itself",
	"internal/cli/cli.go:Out":                  "the escaping itself",
	"internal/cli/cli.go:Line":                 "JSON, which a script reads",
	"internal/cli/cli.go:writeEnvelope":        "JSON, which a script reads",
	"internal/cli/cli.go:fail":                 "the message goes through shownMessage; help is this program's",
	"internal/cli/cli.go:Run":                  "this program's version",
	"internal/cli/cli.go:printHelp":            "this program's help",
	"cmd/localbase/usage.go:printUsage":        "a table's writer; each outside value goes through Printable",
	"cmd/localbase/models.go:progressReporter": "a percentage",
	"cmd/localbase/logs.go:logsCommand":        "--json lines, unchanged, which a script reads",
}

// keepsLineBreaks is every value printed with line breaks of its own: documents this program encoded.
var keepsLineBreaks = map[string]string{
	"cmd/localbase/config.go:runConfigShow":        "TOML, which writes a line break in a value as \\n",
	"cmd/localbase/imports.go:modelsExportCommand": "JSON",
	"cmd/localbase/imports.go:backendsCommand":     "JSON",
	"cmd/localbase/connect.go:printClient":         "snippets a person copies, from this program's templates",
}

// writesItsOwnLines is everywhere that builds a cli.TextError, which prints unescaped. Each one escapes
// its values when it builds the text.
var writesItsOwnLines = map[string]string{
	"internal/install/install.go:downloadError": "the source, with PrintableASCII",
	"internal/install/install.go:Verify":        "the filename and expected digest, with Printable; the other digest is ours",
}

// A TextError prints as it is, on trust, so a new one fails until someone has read it.
func TestEveryTextErrorIsNamed(t *testing.T) {
	seen := map[string]bool{}
	err := filepath.WalkDir("../..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(filepath.ToSlash(path), "../../")
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			where := name + ":" + fn.Name.Name
			ast.Inspect(fn, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if text := exprText(lit.Type); text != "cli.TextError" && text != "TextError" {
					return true
				}
				seen[where] = true
				if writesItsOwnLines[where] == "" {
					t.Errorf("%s:%d: builds a TextError, which prints unescaped; escape its values and name it in writesItsOwnLines", where, fset.Position(lit.Pos()).Line)
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for where := range writesItsOwnLines {
		if !seen[where] {
			t.Errorf("writesItsOwnLines names %s, which no longer builds one", where)
		}
	}
}

var standardStream = regexp.MustCompile(`\.Std(out|err)$`)

// Every value on a terminal is escaped, so a stored name can't move the cursor or fake a line. That
// holds only if nothing prints around Info and Error, so this reads the source for anything that does.
// Formats have to be literal for the same reason: Info doesn't escape its format.
func TestEveryPrintedValueIsEscaped(t *testing.T) {
	var files []string
	for _, dir := range []string{".", "../../internal/cli"} {
		found, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, found...)
	}
	seenDirect, seenLines := map[string]bool{}, map[string]bool{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		name := "cmd/localbase/" + filepath.Base(file)
		if strings.Contains(file, "internal/cli") {
			name = "internal/cli/" + filepath.Base(file)
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			where := name + ":" + fn.Name.Name
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				line := fset.Position(call.Pos()).Line
				recv := exprText(sel.X)
				switch {
				case recv == "c" && (sel.Sel.Name == "Info" || sel.Sel.Name == "Error" || sel.Sel.Name == "Out"):
					if len(call.Args) > 0 && !isStringLiteral(call.Args[0]) {
						t.Errorf("%s:%d: c.%s's format isn't a literal, and formats aren't escaped", name, line, sel.Sel.Name)
					}
				case recv == "cli" && sel.Sel.Name == "Lines":
					seenLines[where] = true
					if keepsLineBreaks[where] == "" {
						t.Errorf("%s:%d: cli.Lines isn't named in keepsLineBreaks", where, line)
					}
				case recv == "fmt" && strings.HasPrefix(sel.Sel.Name, "Fprint") && len(call.Args) > 0:
					w := exprText(call.Args[0])
					toTerminal := standardStream.MatchString(w) || w == "w"
					if !toTerminal || allLiterals(call.Args[1:]) {
						return true
					}
					seenDirect[where] = true
					if printsDirectly[where] == "" {
						t.Errorf("%s:%d: prints directly to %s; use c.Info or c.Error, or name it in printsDirectly", where, line, w)
					}
				case sel.Sel.Name == "Write" && standardStream.MatchString(recv):
					seenDirect[where] = true
					if printsDirectly[where] == "" {
						t.Errorf("%s:%d: writes directly to %s; name it in printsDirectly", where, line, recv)
					}
				}
				return true
			})
		}
	}
	// An entry nothing needs any more would let the next print through unread.
	for where := range printsDirectly {
		if !seenDirect[where] {
			t.Errorf("printsDirectly names %s, which no longer prints directly", where)
		}
	}
	for where := range keepsLineBreaks {
		if !seenLines[where] {
			t.Errorf("keepsLineBreaks names %s, which no longer uses cli.Lines", where)
		}
	}
}

func exprText(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprText(x.X) + "." + x.Sel.Name
	}
	return ""
}

func isStringLiteral(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING
}

func allLiterals(args []ast.Expr) bool {
	for _, a := range args {
		if !isStringLiteral(a) {
			return false
		}
	}
	return true
}

// hostileValues are what a stored name or an argument might hold, from Security's review.
var hostileValues = map[string]string{
	"erase and a fake line": "x\x1b[2K\nFAKE-LINE",
	"c1 control":            "x\u009b1A",
	"reversed text":         "x\u202ey",
	"line separator":        "x\u2028y",
	"not utf-8":             "x\xffy",
	"nul":                   "a\x00b",
}

// unsafeOnATerminal says what in printed text could still move the cursor, reorder it or fake a line.
func unsafeOnATerminal(out string) string {
	for _, r := range []string{"\x1b", "\x07", "\u009b", "\u202e", "\u2028", "\xff", "\x00"} {
		if strings.Contains(out, r) {
			return strconv.Quote(r)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAKE-LINE") {
			return "a value started its own line"
		}
	}
	return ""
}

// Each command that prints a value someone else chose, given one: as an argument here, or stored in
// the root by the step before. A command may refuse it, or fail. Either way what it prints is safe.
// Commands that need the network or a running gateway to reach the value aren't here: hub search, and
// the gateway's half of status, doctor and usage.
func TestCommandsPrintHostileValuesSafely(t *testing.T) {
	commands := func(root, hostile string) [][]string {
		return [][]string{
			{"keys", "create", "--name", hostile},
			{"keys", "list"},
			{"keys", "revoke", hostile},
			{"keys", "rotate", hostile},
			{"keys", "scopes", hostile},
			{"configure", "--llm-models", hostile},
			{"connect", hostile},
			{"connect", "curl", "--create-key", "--name", hostile},
			{"access", "show"},
			{"access", "users", "list"},
			{"access", "users", "show", hostile},
			{"access", "email", "show"},
			{"access", "policy", "show"},
			{"access", "bindings"},
			{"local", "sessions", "list"},
			{"models", "list"},
			{"models", "show", hostile},
			{"models", "export", hostile},
			{"models", "import", hostile},
			{"feed", "status"},
			{"feed", "use", hostile},
			{"backends", "list"},
			{"backends", "show", hostile},
			{"auth-profiles", "list"},
			{"voices", "list"},
			{"usage"},
			{"status"},
			{"doctor"},
			{"service", "status"},
			{"logs"},
			{"diagnostics", "--output", filepath.Join(root, "bundle-"+strconv.Itoa(len(hostile))+".zip")},
			{hostile},
		}
	}
	for name, hostile := range hostileValues {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("LOCALBASE_ROOT", "")
			root := filepath.Join(t.TempDir(), "root")
			if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
				t.Fatalf("configure: %s", out)
			}
			for _, args := range commands(root, hostile) {
				_, out := runApp(t, append([]string{"--root", root}, args...)...)
				// Only the line it's in: some commands print a new key's secret.
				if what := unsafeOnATerminal(out); what != "" {
					t.Errorf("%q printed %s, in: %.200q", args, what, lineWith(out, what))
				}
			}
		})
	}
}

// Ordinary text in another alphabet, and a backslash someone wrote, are left alone.
func TestCommandsLeaveOrdinaryTextAlone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LOCALBASE_ROOT", "")
	root := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	for _, name := range []string{"日本語", `written \x1b out`} {
		if code, out := runApp(t, "--root", root, "keys", "create", "--name", name); code != 0 {
			t.Fatalf("keys create %q: %s", name, out)
		}
		if _, out := runApp(t, "--root", root, "keys", "list"); !strings.Contains(out, name) {
			t.Errorf("keys list doesn't show %q as it is:\n%s", name, out)
		}
	}
}

// A root's path comes back in about 17 messages. It's shown as it is, and nothing in it acts on the
// terminal, including the kind of sequence redaction would have removed.
func TestARootPathHoldingControlCharactersIsShownNotObeyed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LOCALBASE_ROOT", "")
	for name, tc := range map[string]struct{ in, want string }{
		"erase line": {"x\x1b[2Ky", `x\x1b[2Ky`},
		"osc":        {"x\x1b]0;x\x07y", `x\x1b]0;x\x07y`},
		"bare esc":   {"x\x1by", `x\x1by`},
		"c1":         {"x\u009b1Ay", `x\x9b1Ay`},
	} {
		t.Run(name, func(t *testing.T) {
			// A file where the root should be: the refusal names the path.
			path := filepath.Join(t.TempDir(), tc.in)
			if err := os.WriteFile(path, []byte("not a root"), 0o600); err != nil {
				t.Fatal(err)
			}
			code, out := runApp(t, "--root", path, "keys", "list")
			if code == 0 {
				t.Fatalf("it used a file as a root: %s", out)
			}
			if what := unsafeOnATerminal(out); what != "" {
				t.Errorf("printed %s: %q", what, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("doesn't show the path as %s: %q", tc.want, out)
			}
		})
	}
}

// lineWith is the line of out holding what unsafeOnATerminal found, so a failure shows that and not
// everything a command printed.
func lineWith(out, what string) string {
	for _, line := range strings.Split(out, "\n") {
		if unsafeOnATerminal(line) == what || strings.HasPrefix(strings.TrimSpace(line), "FAKE-LINE") {
			return line
		}
	}
	return ""
}
