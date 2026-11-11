// Package treewalk is the walk the source-reading guards share: rmguard, killguard and the
// browser suite's Chrome keychain check. Each guard keeps its own rule, its own file filter and
// its own floor. This package reads the files the filter takes, names what the rule finds by tree,
// and fails a guard that read fewer files than its floor, so a tree that moves out of reach fails
// the guard rather than passing it on less.
//
// It is for tests. No file outside go/test and go/tools may import it, which this package's own
// test checks.
package treewalk

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
)

// Tree is one directory a guard reads, and the label its findings are named with, such as
// "tools" for go/tools.
type Tree struct{ Label, Dir string }

// Filter says which files a guard reads.
type Filter struct {
	// SkipDirs are directory names the walk does not enter, wherever they are.
	SkipDirs []string
	// SkipTests leaves out _test.go files.
	SkipTests bool
	// RegularOnly leaves out anything that is not a regular file, such as a symlink.
	RegularOnly bool
	// Exts are the extensions read, with their dot. A file with no extension is read when it
	// starts with #!.
	Exts []string
	// Makefile reads files named Makefile.
	Makefile bool
}

// Walk reads every file in fsys that f takes and runs rule on its text. It returns what rule
// found, keyed by the file's name in fsys, and how many files it read.
func Walk(fsys fs.FS, f Filter, rule func(text string) []string) (map[string][]string, int, error) {
	found := map[string][]string{}
	read := 0
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name != "." && slices.Contains(f.SkipDirs, path.Base(name)) {
				return fs.SkipDir
			}
			return nil
		}
		if f.RegularOnly && !d.Type().IsRegular() {
			return nil
		}
		if f.SkipTests && strings.HasSuffix(name, "_test.go") {
			return nil
		}
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		switch ext := path.Ext(name); {
		case slices.Contains(f.Exts, ext):
		case f.Makefile && path.Base(name) == "Makefile":
		case ext == "" && strings.HasPrefix(string(b), "#!"):
		default:
			return nil
		}
		read++
		if lines := rule(string(b)); len(lines) > 0 {
			found[name] = lines
		}
		return nil
	})
	return found, read, err
}

// WalkTrees walks each tree in turn and names what rule finds label/name. When floor is above
// zero and the trees held fewer files than floor, it fails t: a tree stopped being read, or files
// moved where none of them reaches. Pass 0 to read a tree other than the guard's own.
func WalkTrees(t testing.TB, trees []Tree, floor int, f Filter, rule func(text string) []string) map[string][]string {
	t.Helper()
	found := map[string][]string{}
	read := 0
	var where []string
	for _, tr := range trees {
		got, n, err := Walk(os.DirFS(tr.Dir), f, rule)
		if err != nil {
			t.Fatal(err)
		}
		for name, lines := range got {
			found[tr.Label+"/"+name] = lines
		}
		read += n
		where = append(where, "go/"+tr.Label)
	}
	if floor > 0 && read < floor {
		t.Errorf("%s", FloorMessage(read, floor, where))
	}
	t.Logf("read %d files (floor %d)", read, floor)
	return found
}

// FloorMessage says that a walk of where read fewer files than its floor.
func FloorMessage(read, floor int, where []string) string {
	return fmt.Sprintf("read %d files in %s, fewer than the %d they held: a tree stopped being read, or files moved where none of them reaches", read, strings.Join(where, " and "), floor)
}

// Names are the keys of found, sorted.
func Names(found map[string][]string) []string {
	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
