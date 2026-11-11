// Package killguard holds a check: nothing under tools/ may end a process by name or pattern, by
// port, or by asking the service manager, and nothing may stop a gateway without saying which root.
// On a machine with a live gateway, other threads' runs and about 78 worktrees, a pattern that looks
// narrow can reach any of them. End a process by a pid you started, or by your own process group.
//
//	go test -count=1 ./tools/killguard
//
// It reads files, so it cannot see a command typed straight into a shell.
package killguard

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

var byNameOrService = []*regexp.Regexp{
	regexp.MustCompile(`\bpkill\b`),
	regexp.MustCompile(`\bkillall\b`),
	regexp.MustCompile(`\blaunchctl"?,?\s+"?(bootout|unload|stop|kill|kickstart|remove|disable)\b`),
	regexp.MustCompile(`\bsystemctl\b.*\b(stop|restart|kill|disable)\b`),
	regexp.MustCompile(`\blsof\b.*\s-t\b`),
	regexp.MustCompile(`\bfuser\s+-k\b`),
}

var stopsAGateway = regexp.MustCompile(`\blocal-base["']?,?\s+"?(stop|restart)\b`)

// findings lists the lines in one file's text that the rule does not allow.
func findings(text string) []string {
	var out []string
	for n, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		for _, re := range byNameOrService {
			if re.MatchString(line) {
				out = append(out, fmt.Sprintf("line %d: %s", n+1, trimmed))
				break
			}
		}
		if stopsAGateway.MatchString(line) && !strings.Contains(line, "--root") {
			out = append(out, fmt.Sprintf("line %d: stops a gateway without --root: %s", n+1, trimmed))
		}
	}
	return out
}

func scan(fsys fs.FS) (map[string][]string, error) {
	found, _, err := scanRead(fsys)
	return found, err
}

// scanRead is scan, and how many files it read.
func scanRead(fsys fs.FS) (map[string][]string, int, error) {
	found := map[string][]string{}
	read := 0
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if base := path.Base(name); base == "node_modules" || base == "testdata" || base == ".git" || base == "killguard" {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
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
		read++
		if f := findings(string(b)); len(f) > 0 {
			found[name] = f
		}
		return nil
	})
	return found, read, err
}

// trees are what the guard reads: go/tools, and go/test, where step 7 moves the test suites and
// their scripts out of go/tools. A move from one to the other changes neither what is read nor how
// much. Findings are named from the go directory, tools/... or test/....
var trees = []struct{ label, dir string }{{"tools", ".."}, {"test", "../../test"}}

// minRead is how many files the trees held when it was last raised. A tree that stops being read,
// or files moved somewhere neither tree reaches, read fewer, and the guard fails. Raise it as files
// arrive.
const minRead = 127

// known are what already does this, with how many lines each has. A file may not gain one, and no
// other file may join. An entry can be absent from a tree: browser-run.sh's are being fixed on another
// branch, and not every branch holds it. Lower a number, or remove a line, as they are fixed.
var known = map[string]int{
	"test/browser/browser-run.sh":     2, // pkill -f on the run's own profile path
	"tools/rehearsal.sh":              1, // the scratch root's own label; it refuses the live one
	"tools/upgrade-rehearsal/main.go": 2, // a scratch root's service, one line per platform; it checks the label against the live ones first
}

func TestNothingStopsAProcessByNameOrAServiceOrWithoutARoot(t *testing.T) {
	roots := trees
	other := os.Getenv("LOCALBASE_KILL_GUARD_TREE")
	if other != "" {
		roots = []struct{ label, dir string }{{"tools", other}} // laid out as go/tools is
	}
	found := map[string][]string{}
	read := 0
	for _, r := range roots {
		f, n, err := scanRead(os.DirFS(r.dir))
		if err != nil {
			t.Fatal(err)
		}
		for name, lines := range f {
			found[r.label+"/"+name] = lines
		}
		read += n
	}
	if other == "" && read < minRead {
		t.Errorf("read %d files in go/tools and go/test, fewer than the %d they held: a tree stopped being read, or files moved where neither reaches", read, minRead)
	}
	t.Logf("read %d files (floor %d)", read, minRead)
	var names []string
	for name := range found {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if n, ok := known[name]; ok && len(found[name]) <= n {
			continue
		}
		t.Errorf("%s ends a process in a way that can reach something else. Use a pid you started, or your own process group:\n  %s", name, strings.Join(found[name], "\n  "))
	}
}

// The check can fail: each shape it exists for is planted, and what it must leave alone.
func TestGuardFindsEachBadShape(t *testing.T) {
	for _, c := range []struct {
		line string
		bad  bool
	}{
		{`pkill -f chat-runs-check`, true},
		{`pkill -9 -f -- "$profile" 2>/dev/null`, true},
		{`killall local-base`, true},
		{`sleep 1; killall -9 llama-server`, true},
		{`exec.Command("pkill", "-f", pattern)`, true},
		{`await Bun.$` + "`pkill -f ${name}`;", true},
		{`launchctl bootout "gui/$(id -u)/$label"`, true},
		{`launchctl unload ~/Library/LaunchAgents/x.plist`, true},
		{`launchctl kickstart -k gui/501/x`, true},
		{`exec.Command("launchctl", "bootout", target)`, true},
		{`systemctl --user stop local-base`, true},
		{`kill -9 $(lsof -nP -iTCP:2273 -t)`, true},
		{`fuser -k 2273/tcp`, true},
		{`local-base stop`, true},
		{`local-base restart`, true},
		{`exec.Command("local-base", "stop")`, true},
		{`local-base --root "$root" stop`, false},
		{`"$bin" --root "$root" stop`, false},
		{`local-base stop --root "$root"`, false},
		{`exec.Command(bin, "--root", g.Root, "stop")`, false},
		{`kill -TERM "$pid"`, false},
		{`kill -TERM -- "-$pid" 2>/dev/null`, false},
		{`syscall.Kill(p.PID, syscall.SIGKILL)`, false},
		{`docker kill "$name"`, false},
		{`launchctl print "gui/501/x"`, false},
		{`lsof -nP -iTCP:29000-29499 -sTCP:LISTEN`, false},
		{`systemctl --user status local-base`, false},
		{`# pkill -f something`, false},
		{`// launchctl bootout x`, false},
		{`echo "backfill all of it"`, false},
	} {
		if got := len(findings(c.line)) > 0; got != c.bad {
			t.Errorf("%q: found %v, want %v", c.line, got, c.bad)
		}
	}
}

// It reads what it should, skips what it should, and a new file is not excused by a known one's name.
func TestScanCoversScriptsAndCode(t *testing.T) {
	found, err := scan(fstest.MapFS{
		"a.sh":             {Data: []byte("pkill -f x\n")},
		"sub/b_test.go":    {Data: []byte(`exec.Command("killall", "x")` + "\n")},
		"c.ts":             {Data: []byte("killall x\n")},
		"noext":            {Data: []byte("#!/bin/sh\npkill x\n")},
		"plain":            {Data: []byte("pkill x\n")},
		"README.md":        {Data: []byte("pkill x\n")},
		"testdata/d.sh":    {Data: []byte("pkill x\n")},
		"killguard/e.go":   {Data: []byte("pkill x\n")},
		"sub/rehearsal.sh": {Data: []byte("pkill x\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for name := range found {
		names = append(names, name)
	}
	slices.Sort(names)
	want := []string{"a.sh", "c.ts", "noext", "sub/b_test.go", "sub/rehearsal.sh"}
	if !slices.Equal(names, want) {
		t.Errorf("found %v, want %v", names, want)
	}
	if _, ok := known["sub/rehearsal.sh"]; ok {
		t.Error("the known list is by path, so this one must not be excused")
	}
}
