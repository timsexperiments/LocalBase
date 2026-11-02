package main

import (
	"os"
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
