package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// labMarkers are what the reliability lab's seam (the localbase_lab build tag)
// adds to a binary: its environment variables and the pprof handlers.
var labMarkers = []string{"LOCALBASE_LAB_HOST_FILE", "LOCALBASE_LAB_DEBUG_ADDR", "net/http/pprof."}

// TestDefaultBuildHasNoLabSeam builds the release shape of the binary and looks
// for the seam in it, and in the file list go uses: a fake memory host and a
// debug listener must exist only in a localbase_lab build.
func TestDefaultBuildHasNoLabSeam(t *testing.T) {
	files := func(tags ...string) string {
		args := append([]string{"list", "-deps", "-f", "{{.ImportPath}}: {{.GoFiles}}"}, tags...)
		out, err := exec.Command("go", append(args, ".")...).CombinedOutput()
		if err != nil {
			t.Fatalf("go list: %v\n%s", err, out)
		}
		return string(out)
	}
	seam := []string{"host_lab.go", "lab_debug.go"}
	plain, tagged := files(), files("-tags", "localbase_lab")
	for _, name := range seam {
		if strings.Contains(plain, name) {
			t.Errorf("the default build compiles %s", name)
		}
		// The check means nothing if the seam's files have been renamed.
		if !strings.Contains(tagged, name) {
			t.Errorf("a localbase_lab build does not compile %s: update this test", name)
		}
	}
	bin := filepath.Join(t.TempDir(), "local-base")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range labMarkers {
		if bytes.Contains(raw, []byte(marker)) {
			t.Errorf("the default binary contains %q", marker)
		}
	}
}

// TestLabSeamFilesAreTagged checks every source file naming the seam's
// environment variables is built only with the tag.
func TestLabSeamFilesAreTagged(t *testing.T) {
	for _, dir := range []string{".", "../../internal"} {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if bytes.Contains(src, []byte("LOCALBASE_LAB_")) && !bytes.HasPrefix(src, []byte("//go:build localbase_lab\n")) {
				t.Errorf("%s names the lab seam without //go:build localbase_lab on its first line", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
