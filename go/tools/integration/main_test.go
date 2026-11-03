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
