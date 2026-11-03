// Package rmguard holds a check: no script or command file may delete through a variable, a
// glob, or a path outside an explicit scratch prefix. Run it before every commit:
//
//	go test -count=1 ./tools/rmguard
//
// LOCALBASE_RM_GUARD_DIRS adds directories of command files, separated by colons, and
// LOCALBASE_RM_GUARD_TREE scans another checkout's go/tools instead of this one, reporting
// only what is not on the list below. It reads files, so it cannot see a command typed
// straight into a shell.
package rmguard

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// scratchPrefixes are where an absolute literal path may be deleted.
var scratchPrefixes = []string{"/private/tmp/claude-", "/private/var/folders/"}

var (
	rmCommand = regexp.MustCompile("(?:^|[\\s;&|(){'\"`])rm((?:\\s+-[a-zA-Z-]+)*)\\s+([^\\n]*)")
	rmInCode  = regexp.MustCompile(`"rm"`)
	otherRm   = regexp.MustCompile(`\b(?:docker|podman|git|kubectl|npm|bun|brew)(?:\s+-\S+)*\s+$`)
)

// badTarget says what is wrong with one target of rm, or "".
func badTarget(target string) string {
	t := strings.Trim(target, `'"`)
	switch {
	case t == "":
		return ""
	case strings.ContainsAny(t, "$`"):
		return "a variable"
	case strings.ContainsAny(t, "*?["):
		return "a glob"
	case strings.HasPrefix(t, "~"):
		return "the home directory"
	case strings.HasPrefix(t, "/dev"):
		return "/dev"
	case strings.HasPrefix(t, ".."):
		return "a path above this one"
	case strings.HasPrefix(t, "/"):
		for _, p := range scratchPrefixes {
			if strings.HasPrefix(t, p) {
				return ""
			}
		}
		return "an absolute path outside scratch"
	}
	return ""
}

// findings lists the deletes in one file's text that the rule does not allow.
func findings(text string) []string {
	var out []string
	for n, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		if rmInCode.MatchString(line) {
			out = append(out, fmt.Sprintf("line %d: rm run from code", n+1))
		}
		for _, m := range rmCommand.FindAllStringSubmatchIndex(line, -1) {
			if otherRm.MatchString(line[:m[0]+1]) { // docker rm and the like remove something else
				continue
			}
			rest := line[m[4]:m[5]]
			if i := strings.IndexAny(rest, ";&|)}"); i >= 0 {
				rest = rest[:i]
			}
			for _, target := range strings.Fields(rest) {
				if strings.HasPrefix(target, "2>") || strings.HasPrefix(target, ">") {
					break
				}
				if why := badTarget(target); why != "" {
					out = append(out, fmt.Sprintf("line %d: rm %s: %s", n+1, target, why))
				}
			}
		}
	}
	return out
}

func scan(fsys fs.FS) (map[string][]string, error) {
	found := map[string][]string{}
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if base := path.Base(name); base == "node_modules" || base == "testdata" || base == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(name, "_test.go") {
			return nil
		}
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		switch ext := path.Ext(name); {
		case ext == ".sh", ext == ".go", ext == ".ts", ext == ".js", ext == ".mjs", ext == ".py", ext == ".pl", path.Base(name) == "Makefile":
		case ext == "" && strings.HasPrefix(string(b), "#!"):
		default:
			return nil
		}
		if f := findings(string(b)); len(f) > 0 {
			found[name] = f
		}
		return nil
	})
	return found, err
}

// knownToDelete are other threads' scripts that already delete through a variable, with how
// many such deletes each has. They are reported, not changed here. A file may not gain one,
// and no other file may join. Lower a number, or remove a line, as they are fixed.
var knownToDelete = map[string]int{
	"axe-check.sh":                 2,
	"browser-run.sh":               3,
	"contract.sh":                  1,
	"ios-check.sh":                 2,
	"otelcol-check.sh":             1,
	"placement-rollback-check.sh":  1,
	"rehearsal.sh":                 1,
	"shadow.sh":                    6,
	"sync-catalog.sh":              1,
	"systemd-check.sh":             1,
	"tailwind.sh":                  1,
	"ui-shots/run.ts":              1,
	"upgrade-rehearsal-systemd.sh": 1,
}

func TestNothingDeletesThroughAVariableOrOutsideScratch(t *testing.T) {
	tree := ".."
	other := os.Getenv("LOCALBASE_RM_GUARD_TREE")
	if other != "" {
		tree = other
	}
	found, err := scan(os.DirFS(tree))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for name := range found {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if n, ok := knownToDelete[name]; ok && len(found[name]) <= n {
			if other == "" && len(found[name]) < n {
				t.Errorf("tools/%s now has %d, so lower its number in knownToDelete from %d", name, len(found[name]), n)
			}
			continue
		}
		t.Errorf("tools/%s:\n  %s", name, strings.Join(found[name], "\n  "))
	}
	if other == "" {
		for name := range knownToDelete {
			if _, ok := found[name]; !ok {
				t.Errorf("tools/%s no longer deletes that way: remove it from knownToDelete", name)
			}
		}
	}
	for _, dir := range filepath.SplitList(os.Getenv("LOCALBASE_RM_GUARD_DIRS")) {
		found, err := scan(os.DirFS(dir))
		if err != nil {
			t.Fatal(err)
		}
		for name, f := range found {
			t.Errorf("%s/%s:\n  %s", dir, name, strings.Join(f, "\n  "))
		}
	}
}

// The check can fail: each shape it exists for is planted, and what it must leave alone.
func TestGuardFindsEachBadShape(t *testing.T) {
	for _, c := range []struct {
		line string
		bad  bool
	}{
		{`rm -rf "$work"`, true},
		{`rm -f $W/x`, true},
		{`rm -rf ${dir}/y`, true},
		{"rm -f `pwd`/z", true},
		{`rm -rf /tmp/*`, true},
		{`rm -f foo?.txt`, true},
		{`rm -f /dev/null`, true},
		{`rm -rf /dev/shm/none 2>/dev/null`, true},
		{`rm -rf /`, true},
		{`rm -rf ~/x`, true},
		{`rm -f ../x`, true},
		{`rm -rf /etc/x`, true},
		{`cd x; rm -rf "$a" b`, true},
		{`trap 'kill $pid; rm -rf "$profile"' EXIT`, true},
		{`[ -n x ] && rm -f "$tmp"`, true},
		{`exec.Command("rm", "-rf", dir)`, true},
		{"await Bun.$`rm -f ${out}/x.html`;", true},
		{`rm -rf /private/tmp/claude-501/x/scratchpad/rn-a`, false},
		{`rm -rf /private/var/folders/2s/T/lblab-123 /private/tmp/claude-501/y`, false},
		{`rm -f plain-file.txt`, false},
		{`rm -rf /private/tmp/claude-501/$x`, true},
		{`docker rm -f "$name"`, false},
		{`git rm -f "$name"`, false},
		{`# rm -rf "$work"`, false},
		{`// rm -rf "$work"`, false},
		{`echo "firm $x"`, false},
		{`format --rm "$x"`, false},
	} {
		if got := len(findings(c.line)) > 0; got != c.bad {
			t.Errorf("%q: found %v, want %v: %v", c.line, got, c.bad, findings(c.line))
		}
	}
	got, err := scan(fstest.MapFS{
		"a.sh":          {Data: []byte("rm -rf \"$x\"\n")},
		"b":             {Data: []byte("#!/bin/sh\nrm -rf \"$x\"\n")},
		"Makefile":      {Data: []byte("clean:\n\trm -rf $(OUT)\n")},
		"notes.md":      {Data: []byte("rm -rf \"$x\"\n")},
		"testdata/c.sh": {Data: []byte("rm -rf \"$x\"\n")},
		"d_test.go":     {Data: []byte("rm -rf \"$x\"\n")},
		"good.sh":       {Data: []byte("rm -f plain\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for n := range got {
		names = append(names, n)
	}
	slices.Sort(names)
	if want := []string{"Makefile", "a.sh", "b"}; !slices.Equal(names, want) {
		t.Errorf("found %q, want %q", names, want)
	}
}
