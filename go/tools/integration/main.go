// Command integration rebuilds the integration branch from the ordered list in
// order.txt, in a scratch worktree, so the whole tree is one command:
//
//	go run ./tools/integration              # build and report
//	go run ./tools/integration -test        # also gofmt, vet and the unit tests
//	go run ./tools/integration -push        # also force-push with a lease
//
// It merges each branch with no fast-forward. A conflict goes first to git rerere,
// which replays a resolution recorded earlier. Files in unionFiles keep both sides,
// since their conflicts are sections added side by side. Anything else stops with the
// worktree left as it is: resolve it, commit, and run again with -resume; rerere
// remembers the resolution for every later rebuild.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const prefix = "rewrite/go-"

var unionFiles = map[string]bool{
	"go/DESIGN.md": true, "go/docs/merge-readiness.md": true, "CHANGELOG.md": true, "go/contract/fsguard_test.go": true,
}

// generatedCSS is built from the templates, so a conflict in it means nothing: take
// either side and rebuild it when the merges are done.
const generatedCSS = "go/internal/web/static/app.css"

const trailer = "\n\nCo-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>\n"

func main() {
	target := flag.String("target", prefix+"integration-2", "the branch to rebuild")
	base := flag.String("base", prefix+"merge-readiness", "the branch it starts from")
	dir := flag.String("worktree", filepath.Join(os.TempDir(), "lb-integration"), "where to build it")
	push := flag.Bool("push", false, "force-push the result with a lease")
	test := flag.Bool("test", false, "run gofmt, vet and the unit tests on the result")
	resume := flag.Bool("resume", false, "continue in the existing worktree after resolving a conflict")
	flag.Parse()
	list, err := readOrder(orderPath())
	check(err)
	check(run("", "git", "fetch", "-q", "origin"))
	if !*resume {
		// Only ever removes this tool's own worktree.
		_ = exec.Command("git", "worktree", "remove", "--force", *dir).Run()
		check(run("", "git", "worktree", "prune"))
		check(run("", "git", "worktree", "add", "-q", "--force", "-B", *target, *dir, "origin/"+*base))
	}
	for _, kv := range [][2]string{{"rerere.enabled", "true"}, {"rerere.autoupdate", "false"}} {
		check(run(*dir, "git", "config", kv[0], kv[1]))
	}
	for _, b := range list {
		if branch, from, ok := parsePick(b); ok {
			ref := "origin/" + prefix + branch
			if picked(*dir, ref, from) {
				fmt.Printf("have    %s\n", branch)
				continue
			}
			cmdErr := exec.Command("git", "-C", *dir, "cherry-pick", "--empty=drop", "--first-parent", "--no-merges", from+".."+ref).Run()
			for cmdErr != nil && exec.Command("git", "-C", *dir, "rev-parse", "--verify", "-q", "CHERRY_PICK_HEAD").Run() == nil {
				left, err := resolve(*dir)
				check(err)
				if len(left) > 0 {
					fatalf("picking %s stopped in %s. Unresolved:\n  %s\nResolve them, git cherry-pick --continue, then run again with -resume.",
						branch, *dir, strings.Join(left, "\n  "))
				}
				cmdErr = exec.Command("git", "-C", *dir, "-c", "core.editor=true", "cherry-pick", "--continue").Run()
			}
			check(cmdErr)
			fmt.Printf("picked  %s\n", branch)
			continue
		}
		ref := "origin/" + prefix + b
		if exec.Command("git", "-C", *dir, "rev-parse", "--verify", "-q", ref).Run() != nil {
			fatalf("no branch %s", ref)
		}
		if exec.Command("git", "-C", *dir, "merge-base", "--is-ancestor", ref, "HEAD").Run() == nil {
			fmt.Printf("have    %s\n", b)
			continue
		}
		msg := "Merge " + prefix + b + " into the integration tree" + trailer
		if exec.Command("git", "-C", *dir, "merge", "--no-ff", "-q", "-m", msg, ref).Run() == nil {
			fmt.Printf("merged  %s\n", b)
			continue
		}
		left, err := resolve(*dir)
		check(err)
		if len(left) > 0 {
			fmt.Printf("\nSTOPPED at %s. Unresolved in %s:\n  %s\nResolve them, commit, then run again with -resume.\n",
				b, *dir, strings.Join(left, "\n  "))
			os.Exit(1)
		}
		check(run(*dir, "git", "commit", "-q", "-m", msg))
		fmt.Printf("merged  %s (conflicts resolved)\n", b)
	}
	removed, err := tidyDesign(filepath.Join(*dir, "go", "DESIGN.md"))
	check(err)
	check(run(filepath.Join(*dir, "go"), "sh", "-c", `"$(go env GOROOT)/bin/gofmt" -w contract/fsguard_test.go`))
	check(run(filepath.Join(*dir, "go"), "tools/tailwind.sh"))
	if out, _ := exec.Command("git", "-C", *dir, "status", "--porcelain").Output(); len(out) > 0 {
		check(run(*dir, "git", "add", "-A"))
		check(run(*dir, "git", "commit", "-q", "-m", "Tidy what the merges left"+trailer))
	}
	if removed > 0 {
		fmt.Printf("removed %d repeats from DESIGN\n", removed)
	}
	// The differential runs the TS gateway from this tree, and a worktree starts
	// with no node_modules: without them TS never becomes healthy.
	if _, err := exec.LookPath("bun"); err != nil {
		fatalf("bun is not installed; the tree cannot run -impl=both without it")
	}
	check(run(*dir, "bun", "install", "--frozen-lockfile"))
	if *test {
		gosrc := filepath.Join(*dir, "go")
		check(run(gosrc, "sh", "-c", `test -z "$("$(go env GOROOT)/bin/gofmt" -l .)"`))
		check(run(gosrc, "go", "vet", "./..."))
		check(run(gosrc, "go", "test", "./internal/...", "./cmd/...", "-count=1", "-timeout", "400s"))
	}
	if *push {
		check(run(*dir, "git", "push", "--force-with-lease", "origin", *target))
	}
	fmt.Printf("built %s in %s\n", *target, *dir)
}

// resolve handles a stopped merge and returns what still needs a person. rerere has
// already written what it remembers into the tree, but a remembered resolution of a
// union file holds that file's content from an older build: those are rebuilt from
// the conflict itself.
func resolve(dir string) ([]string, error) {
	out, err := exec.Command("git", "-C", dir, "diff", "--name-only", "--diff-filter=U").Output()
	if err != nil {
		return nil, err
	}
	var left []string
	for _, f := range strings.Fields(string(out)) {
		if f == generatedCSS {
			if err := run(dir, "git", "checkout", "--ours", "--", f); err != nil {
				return nil, err
			}
			if err := run(dir, "git", "add", f); err != nil {
				return nil, err
			}
			continue
		}
		path := filepath.Join(dir, f)
		if !unionFiles[f] {
			b, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			if markerRe.Match(b) {
				left = append(left, f)
				continue
			}
		} else {
			if err := run(dir, "git", "checkout", "-m", "--", f); err != nil {
				return nil, err
			}
			if err := keepBothSides(path); err != nil {
				return nil, err
			}
		}
		if err := run(dir, "git", "add", f); err != nil {
			return nil, err
		}
	}
	return left, nil
}

var markerRe = regexp.MustCompile(`(?m)^(<<<<<<<|=======|>>>>>>>)( .*)?\n`)

func keepBothSides(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path, markerRe.ReplaceAll(b, nil), 0o600)
}

// tidyDesign drops a top-level section that exactly repeats an earlier one, which
// keeping both sides produces when two branches carry the same section.
func tidyDesign(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var sections [][]byte
	var cur []byte
	removedHeadings := 0
	var prev []byte
	for _, line := range bytes.SplitAfter(b, []byte("\n")) {
		// Both sides of a renumbered heading, kept one above the other.
		if sameTitle(prev, line) {
			removedHeadings++
			continue
		}
		prev = line
		if bytes.HasPrefix(line, []byte("## ")) && len(cur) > 0 {
			sections = append(sections, cur)
			cur = nil
		}
		cur = append(cur, line...)
	}
	sections = append(sections, cur)
	seen, removed := map[string]bool{}, 0
	var out bytes.Buffer
	for _, s := range sections {
		key := string(bytes.TrimSpace(s))
		if seen[key] {
			removed++
			continue
		}
		seen[key] = true
		out.Write(s)
	}
	if removed+removedHeadings == 0 {
		return 0, nil
	}
	return removed + removedHeadings, os.WriteFile(path, out.Bytes(), 0o600)
}

var numberedHeading = regexp.MustCompile(`^## \d+\. (.*)`)

// sameTitle: two top-level headings that differ only in their number.
func sameTitle(a, b []byte) bool {
	x, y := numberedHeading.FindSubmatch(a), numberedHeading.FindSubmatch(b)
	return x != nil && y != nil && bytes.Equal(x[1], y[1])
}

// picked reports whether a -resume has already replayed this branch: the tree holds a
// commit with the subject of its newest own commit.
func picked(dir, ref, from string) bool {
	tip, err := exec.Command("git", "-C", dir, "log", "-1", "--first-parent", "--no-merges", "--format=%s", from+".."+ref).Output()
	subject := strings.TrimSpace(string(tip))
	if err != nil || subject == "" {
		return false
	}
	out, err := exec.Command("git", "-C", dir, "log", "--format=%s", "-400").Output()
	return err == nil && slices.Contains(strings.Split(string(out), "\n"), subject)
}

// parsePick reads "pick <branch> <from>": a branch built on an earlier integration tree,
// or stacked on such a branch, whose own commits (from..branch along its first parents,
// merges left out) are replayed. A merge would bring that old tree's history back in.
func parsePick(line string) (branch, from string, ok bool) {
	f := strings.Fields(line)
	if len(f) != 3 || f[0] != "pick" {
		return "", "", false
	}
	return f[1], f[2], true
}

func orderPath() string {
	if _, err := os.Stat("tools/integration/order.txt"); err == nil {
		return "tools/integration/order.txt"
	}
	return "go/tools/integration/order.txt"
}

func readOrder(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out, sc.Err()
}

func run(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func check(err error) {
	if err != nil {
		fatalf("%v", err)
	}
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "integration: "+format+"\n", a...)
	os.Exit(1)
}
