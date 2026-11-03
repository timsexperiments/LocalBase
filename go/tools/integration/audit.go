package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// audit reports what a folded pull request added that the tree no longer has. A pick replays
// commits and skips merges, so anything inside a merge commit is lost without a word; a later
// pull request can also rightly replace a line. Each missing line is checked against
// intended.txt, and what is not explained there is a failure.
func audit(dir string, list []string, base string, intended []intention) (unexplained int, report []string) {
	for _, b := range list {
		branch, from := b, ""
		if p, f, ok := parsePick(b); ok {
			branch, from = p, f
		}
		ref := "origin/" + prefix + branch
		if from == "" {
			out, err := exec.Command("git", "-C", dir, "merge-base", "origin/"+base, ref).Output()
			if err != nil {
				report = append(report, fmt.Sprintf("%s: no merge base: %v", branch, err))
				unexplained++
				continue
			}
			from = strings.TrimSpace(string(out))
		}
		missing, err := missingLines(dir, from, ref)
		if err != nil {
			report = append(report, fmt.Sprintf("%s: %v", branch, err))
			unexplained++
			continue
		}
		for _, m := range missing {
			if why := explained(intended, branch, m.file, m.line); why != "" {
				report = append(report, fmt.Sprintf("  intended %s %s: %s (%s)", branch, m.file, clip(m.line), why))
				continue
			}
			if by := replacedBy(dir, m.file, m.line); by != "" {
				report = append(report, fmt.Sprintf("  replaced %s %s: %s (by %s)", branch, m.file, clip(m.line), by))
				continue
			}
			report = append(report, fmt.Sprintf("MISSING %s %s: %s", branch, m.file, clip(m.line)))
			unexplained++
		}
	}
	return unexplained, report
}

// replacedBy names the commit in the tree that took a line out. A line that was once in the tree
// reached it, and something later changed it on purpose; one that never was is what gets lost.
func replacedBy(dir, file, line string) string {
	out, err := exec.Command("git", "-C", dir, "log", "-1", "--no-merges", "--format=%h %s", "-S"+line, "HEAD", "--", file).Output()
	if err != nil {
		return ""
	}
	return clip(strings.TrimSpace(string(out)))
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 110 {
		return s[:110] + "…"
	}
	return s
}

type missingLine struct{ file, line string }

// missingLines is what from..ref added and the worktree's HEAD does not hold, by whole line in
// the same file. Generated files, and lines with too little in them to mean anything, are left out.
func missingLines(dir, from, ref string) ([]missingLine, error) {
	out, err := exec.Command("git", "-C", dir, "diff", "--unified=0", "--no-color", "--no-renames", from, ref).Output()
	if err != nil {
		return nil, fmt.Errorf("diff %s %s: %w", from, ref, err)
	}
	var missing []missingLine
	file := ""
	var have map[string]bool
	seen := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		l := sc.Text()
		if name, ok := strings.CutPrefix(l, "+++ "); ok {
			file, have = "", nil
			if name != "/dev/null" {
				file = strings.TrimPrefix(name, "b/")
			}
			continue
		}
		if strings.HasPrefix(l, "--- ") || file == "" || generatedFile(file) || !strings.HasPrefix(l, "+") {
			continue
		}
		line := strings.TrimSpace(l[1:])
		if len(line) < 4 || seen[file+"\x00"+line] {
			continue
		}
		seen[file+"\x00"+line] = true
		if have == nil {
			have = linesAtHead(dir, file)
		}
		if !have[line] {
			missing = append(missing, missingLine{file, line})
		}
	}
	return missing, sc.Err()
}

func generatedFile(file string) bool {
	switch filepath.Base(file) {
	case "app.css", "bun.lock", "go.sum", "api-reference.md":
		return true
	}
	return strings.Contains(file, "/testdata/")
}

func linesAtHead(dir, file string) map[string]bool {
	have := map[string]bool{}
	out, err := exec.Command("git", "-C", dir, "show", "HEAD:"+file).Output()
	if err != nil {
		return have
	}
	for _, l := range strings.Split(string(out), "\n") {
		have[strings.TrimSpace(l)] = true
	}
	return have
}

// intention is one line of intended.txt: "<branch> <file> <text the line contains> | <why>".
// A branch of * is any branch. A text of * is any line of that file.
type intention struct{ branch, file, text, why string }

func readIntended(path string) ([]intention, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var list []intention
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		spec, why, ok := strings.Cut(l, " | ")
		f := strings.SplitN(spec, " ", 3)
		if !ok || strings.TrimSpace(why) == "" || len(f) != 3 {
			return nil, fmt.Errorf("intended.txt: want \"<branch> <file> <text> | <why>\": %s", l)
		}
		list = append(list, intention{f[0], f[1], f[2], strings.TrimSpace(why)})
	}
	return list, nil
}

func explained(list []intention, branch, file, line string) string {
	for _, i := range list {
		if (i.branch == "*" || i.branch == branch) && (i.file == "*" || i.file == file) && (i.text == "*" || strings.Contains(line, i.text)) {
			return i.why
		}
	}
	return ""
}
