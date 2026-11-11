package treewalk

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

// marked is a rule that reports every line holding the word "marked".
func marked(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "marked") {
			out = append(out, line)
		}
	}
	return out
}

// The filter reads what it names and skips what it names, and the rule runs on what is read.
func TestWalkReadsWhatTheFilterTakes(t *testing.T) {
	file := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	fsys := fstest.MapFS{
		"a.sh":          file("marked\n"),
		"b.go":          file("marked\n"),
		"c_test.go":     file("marked\n"),
		"d.md":          file("marked\n"),
		"Makefile":      file("marked\n"),
		"noext":         file("#!/bin/sh\nmarked\n"),
		"plain":         file("marked\n"),
		"clean.sh":      file("nothing here\n"),
		"skip/e.sh":     file("marked\n"),
		"sub/f.sh":      file("marked\n"),
		"sub/skip/g.sh": file("marked\n"),
	}
	for _, c := range []struct {
		name string
		f    Filter
		want []string
		read int
	}{
		{"everything", Filter{Exts: []string{".sh", ".go"}, Makefile: true},
			[]string{"Makefile", "a.sh", "b.go", "c_test.go", "noext", "skip/e.sh", "sub/f.sh", "sub/skip/g.sh"}, 9},
		{"no tests, no skip dirs, regular only", Filter{SkipDirs: []string{"skip"}, SkipTests: true, RegularOnly: true, Exts: []string{".sh", ".go"}},
			[]string{"a.sh", "b.go", "noext", "sub/f.sh"}, 5},
	} {
		got, read, err := Walk(fsys, c.f, marked)
		if err != nil {
			t.Fatal(err)
		}
		if names := Names(got); !slices.Equal(names, c.want) {
			t.Errorf("%s: found %q, want %q", c.name, names, c.want)
		}
		if read != c.read {
			t.Errorf("%s: read %d files, want %d", c.name, read, c.read)
		}
	}
}

// RegularOnly leaves out a symlink, which is otherwise read through.
func TestRegularOnlySkipsALink(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/a.sh", []byte("marked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.sh", dir+"/link.sh"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		regular bool
		want    []string
	}{{false, []string{"a.sh", "link.sh"}}, {true, []string{"a.sh"}}} {
		got, read, err := Walk(os.DirFS(dir), Filter{RegularOnly: c.regular, Exts: []string{".sh"}}, marked)
		if err != nil {
			t.Fatal(err)
		}
		if names := Names(got); !slices.Equal(names, c.want) || read != len(c.want) {
			t.Errorf("RegularOnly %v: found %q in %d files, want %q", c.regular, names, read, c.want)
		}
	}
}

// A walk that reads fewer files than its floor fails, and says where it looked.
func TestWalkTreesFailsBelowItsFloor(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/a.sh", []byte("marked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := Filter{Exts: []string{".sh"}}
	for _, c := range []struct {
		floor int
		fails bool
	}{{0, false}, {1, false}, {2, true}} {
		rec := &recorder{TB: t}
		found := WalkTrees(rec, []Tree{{"tools", dir}}, c.floor, f, marked)
		if rec.failed != c.fails {
			t.Errorf("floor %d: failed %v, want %v (%q)", c.floor, rec.failed, c.fails, rec.errors)
		}
		if names := Names(found); !slices.Equal(names, []string{"tools/a.sh"}) {
			t.Errorf("floor %d: found %q", c.floor, names)
		}
	}
	if got := FloorMessage(3, 9, []string{"go/tools", "go/test"}); !strings.Contains(got, "read 3 files in go/tools and go/test, fewer than the 9") {
		t.Errorf("message %q", got)
	}
}

// recorder keeps a test's failures to itself, so a failure can be the expected result.
type recorder struct {
	testing.TB
	failed bool
	errors []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.failed = true
	r.errors = append(r.errors, format)
}

func (r *recorder) Logf(string, ...any) {}

// testOnly are packages that exist for tests. No non-test file outside go/test and go/tools may
// import one.
var testOnly = []string{"github.com/timsexperiments/LocalBase/go/test/guards/treewalk"}

// minOutside is how many non-test Go files there were outside go/test and go/tools when it was
// last raised. Fewer means the walk stopped reaching them.
const minOutside = 340

func TestOnlyTestsImportThisPackage(t *testing.T) {
	module := "../../.."
	if _, err := os.Stat(module + "/go.mod"); err != nil {
		t.Fatalf("the go module is not at %s: %v", module, err)
	}
	read := 0
	err := fs.WalkDir(os.DirFS(module), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch {
			case name == "test", name == "tools", path.Base(name) == "node_modules", path.Base(name) == "testdata", path.Base(name) == ".git":
				return fs.SkipDir
			}
			return nil
		}
		if path.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		read++
		file, err := parser.ParseFile(token.NewFileSet(), module+"/"+name, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range file.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if slices.Contains(testOnly, p) {
				t.Errorf("go/%s imports %s, which is for tests in go/test and go/tools only", name, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if read < minOutside {
		t.Errorf("%s", FloorMessage(read, minOutside, []string{"go, outside go/test and go/tools,"}))
	}
	t.Logf("read %d files (floor %d)", read, minOutside)
}
