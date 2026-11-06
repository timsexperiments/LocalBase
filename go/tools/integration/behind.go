package main

import (
	"fmt"
	"os/exec"
	"slices"
	"strings"
)

// behindRow is a folded branch against the tree. A merged branch is in the tree when its head is
// an ancestor. A pick never is, since its commits are replayed, so it is in the tree when every
// commit of its own has a commit with that subject there, and what is inside a merge commit
// is left to the audit.
type behindRow struct {
	branch, kind, head string
	behind             int
	missing            []string
	mergesInPick       int
}

func behind(dir string, list []string) ([]behindRow, error) {
	out, err := exec.Command("git", "-C", dir, "log", "--format=%s", "HEAD").Output()
	if err != nil {
		return nil, err
	}
	have := strings.Split(string(out), "\n")
	var rows []behindRow
	for _, b := range list {
		if b == tidyStep {
			continue
		}
		branch, from, isPick := parsePick(b)
		if !isPick {
			branch = b
		}
		ref := "origin/" + prefix + branch
		head, err := gitLine(dir, "rev-parse", "--short=8", ref)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", branch, err)
		}
		row := behindRow{branch: branch, kind: "merge", head: head}
		if isPick {
			row.kind = "pick"
			list, err := gitLines(dir, "log", "--first-parent", "--no-merges", "--format=%h %s", from+".."+ref)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", branch, err)
			}
			for _, l := range list {
				_, subject, _ := strings.Cut(l, " ")
				if !slices.Contains(have, subject) {
					row.behind++
					row.missing = append(row.missing, l)
				}
			}
			merges, err := gitLines(dir, "log", "--first-parent", "--merges", "--format=%h", from+".."+ref)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", branch, err)
			}
			row.mergesInPick = len(merges)
		} else {
			list, err := gitLines(dir, "log", "--no-merges", "--format=%h %s", "HEAD.."+ref)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", branch, err)
			}
			row.behind, row.missing = len(list), list
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func gitLine(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

func gitLines(dir string, args ...string) ([]string, error) {
	s, err := gitLine(dir, args...)
	if err != nil || s == "" {
		return nil, err
	}
	return strings.Split(s, "\n"), nil
}
