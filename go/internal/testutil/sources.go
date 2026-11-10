package testutil

import (
	"io/fs"
	"path/filepath"
	"strings"
)

// GoSources is every .go file that isn't a test in dir and the directories under it, leaving out
// testdata and hidden directories, in walk order. A test that reads source for something starts
// from here instead of one directory's *.go, so code moved into a package under dir is still read.
// It fails the test unless the files are in at least minPackages directories: a later move that
// takes code out from under dir, or a walk that stops reading, fails instead of checking less.
func GoSources(t Fataler, dir string, minPackages int) []string {
	t.Helper()
	var files []string
	packages := map[string]bool{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != dir && (name == "testdata" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			files = append(files, path)
			packages[filepath.Dir(path)] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("reading the sources under %s: %v", dir, err)
		return nil
	}
	if len(packages) < minPackages {
		t.Fatalf("%s has sources in %d directories, and at least %d were expected: has code moved out from under it?", dir, len(packages), minPackages)
		return nil
	}
	return files
}
