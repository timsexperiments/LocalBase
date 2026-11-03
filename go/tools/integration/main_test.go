package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTidyDesignDropsOnlyExactRepeats(t *testing.T) {
	p := filepath.Join(t.TempDir(), "DESIGN.md")
	in := "# Title\n\n## 1. One\n\ntext\n\n## 2. Two\n\nmore\n\n## 1. One\n\ntext\n\n## 1. One\n\ndifferent\n"
	os.WriteFile(p, []byte(in), 0o600)
	n, err := tidyDesign(p)
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	got, _ := os.ReadFile(p)
	if strings.Count(string(got), "## 1. One") != 2 || !strings.Contains(string(got), "different") {
		t.Errorf("%s", got)
	}
	if n, _ := tidyDesign(p); n != 0 {
		t.Errorf("second pass: %d", n)
	}
}

func TestTidyDesignDropsRenumberedHeading(t *testing.T) {
	p := filepath.Join(t.TempDir(), "DESIGN.md")
	os.WriteFile(p, []byte("## 28. First\n## 25. First\n\nbody\n\n## 25. Other\n\nx\n"), 0o600)
	if n, err := tidyDesign(p); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "## 28. First\n\nbody\n\n## 25. Other\n\nx\n" {
		t.Errorf("%q", got)
	}
}

func TestTidyDesignKeepsTheFullerCopy(t *testing.T) {
	for name, in := range map[string]string{
		"fuller second": "## 1. A\n\ntext\n\n## 2. B\n\nb\n\n## 1. A\n\ntext\n\nadded\n",
		"fuller first":  "## 1. A\n\ntext\n\nadded\n\n## 2. B\n\nb\n\n## 1. A\n\ntext\n",
	} {
		p := filepath.Join(t.TempDir(), "DESIGN.md")
		os.WriteFile(p, []byte(in), 0o600)
		if n, err := tidyDesign(p); err != nil || n != 1 {
			t.Fatalf("%s: %d %v", name, n, err)
		}
		got, _ := os.ReadFile(p)
		if strings.Count(string(got), "## 1. A") != 1 || !strings.Contains(string(got), "added") || !strings.Contains(string(got), "## 2. B") {
			t.Errorf("%s: %q", name, got)
		}
	}
}

func TestKeepBothSides(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	os.WriteFile(p, []byte("a\n<<<<<<< HEAD\nb\n=======\nc\n>>>>>>> origin/x\nd\n"), 0o600)
	if err := keepBothSides(p); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != "a\nb\nc\nd\n" {
		t.Errorf("%q", got)
	}
}

func TestReadOrder(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o")
	os.WriteFile(p, []byte("# c\n\none\n two \n"), 0o600)
	got, err := readOrder(p)
	if err != nil || strings.Join(got, ",") != "one,two" {
		t.Errorf("%v %v", got, err)
	}
}

func TestParsePick(t *testing.T) {
	if b, f, ok := parsePick("pick fix-x abc123"); !ok || b != "fix-x" || f != "abc123" {
		t.Errorf("%q %q %v", b, f, ok)
	}
	if _, _, ok := parsePick("fix-x"); ok {
		t.Error("a plain branch is not a pick")
	}
}

// A branch whose newest commit is one already in the tree, cherry-picked from a branch
// folded earlier, is not done: its other commits are not there. Skipping it whole once
// dropped a pull request from the tree without a word.
func TestPickedNeedsEveryCommit(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.test"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	commit := func(subject string) {
		t.Helper()
		git("commit", "-q", "--allow-empty", "-m", subject)
	}
	git("init", "-q", "-b", "tree")
	commit("base")
	git("branch", "base")
	commit("shared fix")
	git("checkout", "-q", "-b", "pr", "base")
	commit("the pull request's own work")
	commit("shared fix")
	git("checkout", "-q", "tree")
	if picked(dir, "pr", "base") {
		t.Error("a branch with only its newest commit in the tree counts as replayed")
	}
	commit("the pull request's own work")
	if !picked(dir, "pr", "base") {
		t.Error("a branch with every commit in the tree does not count as replayed")
	}
	if picked(dir, "base", "base") {
		t.Error("a branch with no commits of its own counts as replayed")
	}
}

// A pull request's edit inside a merge commit is what a pick drops. The audit sees it, by the
// pull request's whole diff, and a line explained in intended.txt does not fail it.
func TestAuditSeesWhatAMergeHeld(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.test"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		git("add", "f.txt")
	}
	git("init", "-q", "-b", "tree")
	write("start of the file\n")
	git("commit", "-q", "-m", "base")
	git("branch", "base")
	git("checkout", "-q", "-b", "other")
	git("commit", "-q", "--allow-empty", "-m", "other")
	git("checkout", "-q", "-b", "pr", "base")
	write("start of the file\nkept by a plain commit\n")
	git("commit", "-q", "-m", "plain")
	git("merge", "-q", "--no-commit", "--no-ff", "other")
	write("start of the file\nkept by a plain commit\ninside the merge commit\n")
	git("commit", "-q", "-m", "merge with an edit")
	git("checkout", "-q", "tree")
	write("start of the file\nkept by a plain commit\n")
	git("commit", "-q", "-m", "the plain commit, replayed")
	missing, err := missingLines(dir, "base", "pr")
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0].file != "f.txt" || missing[0].line != "inside the merge commit" {
		t.Fatalf("%+v", missing)
	}
	list := []intention{{"pr", "f.txt", "inside the merge", "moved elsewhere"}}
	if explained(list, "pr", "f.txt", missing[0].line) == "" {
		t.Error("an intended line is not explained")
	}
	if explained(list, "another", "f.txt", missing[0].line) != "" || explained(list, "pr", "g.txt", missing[0].line) != "" {
		t.Error("an intention reaches another branch or file")
	}
}

// The tool has rerere on for itself alone: the setting reaches its git commands, and a repository
// that says off stays off for everyone else.
func TestRerereIsTheToolsAlone(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("git", "-C", dir, "init", "-q")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "config", "rerere.enabled", "false").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, k := range []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1"} {
		t.Setenv(k, "")
	}
	useRerere()
	out, err := exec.Command("git", "-C", dir, "config", "rerere.enabled").Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Errorf("the tool's git commands see %q, %v", out, err)
	}
	file, err := os.ReadFile(filepath.Join(dir, ".git", "config"))
	if err != nil || !strings.Contains(string(file), "enabled = false") {
		t.Errorf("the repository's own setting changed: %s %v", file, err)
	}
}

// A branch that moved after the fold shows up, merged or picked, with what it lacks.
func TestBehindSeesNewCommits(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.test"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	commit := func(subject string) { t.Helper(); git("commit", "-q", "--allow-empty", "-m", subject) }
	git("init", "-q", "-b", "tree")
	commit("base")
	git("branch", "base")
	git("checkout", "-q", "-b", "merged", "base")
	commit("merged work")
	git("checkout", "-q", "-b", "picked", "base")
	commit("picked work")
	git("checkout", "-q", "tree")
	git("merge", "-q", "--no-ff", "-m", "merge", "merged")
	commit("picked work")
	for _, b := range []string{"merged", "picked"} {
		git("update-ref", "refs/remotes/origin/"+prefix+b, b)
	}
	list := []string{"merged", "pick picked base", tidyStep}
	rows, err := behind(dir, list)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].behind != 0 || rows[1].behind != 0 {
		t.Fatalf("nothing moved yet: %+v", rows)
	}
	git("checkout", "-q", "merged")
	commit("merged, later")
	git("update-ref", "refs/remotes/origin/"+prefix+"merged", "merged")
	git("checkout", "-q", "picked")
	commit("picked, later")
	git("update-ref", "refs/remotes/origin/"+prefix+"picked", "picked")
	git("checkout", "-q", "tree")
	rows, err = behind(dir, list)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].behind != 1 || !strings.Contains(rows[0].missing[0], "merged, later") || rows[1].behind != 1 || !strings.Contains(rows[1].missing[0], "picked, later") {
		t.Errorf("%+v", rows)
	}
}
