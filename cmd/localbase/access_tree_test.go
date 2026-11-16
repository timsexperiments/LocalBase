package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
)

// accessTree is the access command tree as text: each command's path and every field that shapes it
// (summary, usage, arguments, options, examples, whether it runs or exits with its own codes, and
// the order of its subcommands), then the help each command prints.
func accessTree(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	var walk func(path []string, c *cli.Command)
	walk = func(path []string, c *cli.Command) {
		var subs []string
		for _, s := range c.Sub {
			subs = append(subs, s.Name)
		}
		fmt.Fprintf(&b, "== %s\nsummary %q\nusage %q\nargs %d-%d id=%v\nrun=%v exitCodeFor=%v inputExit=%d\nsubs %v\n",
			strings.Join(path, " "), c.Summary, c.Usage, c.MinArgs, c.MaxArgs, c.IDArg, c.Run != nil, c.ExitCodeFor != nil, c.InputExitCode, subs)
		for _, o := range c.Options {
			fmt.Fprintf(&b, "option %+v\n", o)
		}
		for _, e := range c.Examples {
			fmt.Fprintf(&b, "example %q\n", e)
		}
		code, out := runApp(t, append(append([]string{}, path...), "--help")...)
		fmt.Fprintf(&b, "help exit %d:\n%s\n", code, out)
		for _, s := range c.Sub {
			walk(append(append([]string{}, path...), s.Name), s)
		}
	}
	walk([]string{"access"}, accessCommand())
	return b.String()
}

// The access command tree, every command's fields and help, is exactly the golden one. It pins the
// tree while access.go is split by group (5-5a): a split that dropped, reordered or changed a
// command fails here. LOCALBASE_UPDATE_GOLDEN=1 rewrites the golden.
func TestTheAccessCommandTreeIsUnchanged(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	got := accessTree(t)
	path := filepath.Join("testdata", "access_tree.golden")
	if os.Getenv("LOCALBASE_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("the access command tree changed (LOCALBASE_UPDATE_GOLDEN=1 rewrites testdata/access_tree.golden if that is meant):\n%s", firstDifference(string(want), got))
	}
}

// firstDifference shows where two texts first differ, by line.
func firstDifference(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return fmt.Sprintf("line %d:\nwant %q\ngot  %q", i+1, wl, gl)
		}
	}
	return "(same lines; a difference in line endings)"
}
