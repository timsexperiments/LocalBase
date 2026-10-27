package main

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Run checkpoints are the one place model output touches disk (DESIGN
// §21.6): no diagnostics bundle and no root-copying tool may carry them.
func TestRunCheckpointsStayOutOfBundles(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	root := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	const secret = "runOutputSecret55"
	os.MkdirAll(filepath.Join(root, "runs"), 0o700)
	os.WriteFile(filepath.Join(root, "runs", "run_0123.json"), []byte(`{"id":"run_0123","owner":"local","status":"in_progress",`+
		`"output":{"content":"`+secret+`"}}`), 0o600)
	zipPath := filepath.Join(t.TempDir(), "d.zip")
	if code, out := runApp(t, "--root", root, "--json", "diagnostics", "--output", zipPath); code != 0 {
		t.Fatalf("diagnostics: %s", out)
	} else if strings.Contains(out, secret) {
		t.Error("diagnostics output holds run output")
	}
	raw, _ := os.ReadFile(zipPath)
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		if strings.Contains(f.Name, "runs") || strings.Contains(string(b), secret) {
			t.Errorf("bundle entry %s carries run output", f.Name)
		}
	}
	if strings.Contains(string(raw), secret) {
		t.Error("the archive holds run output")
	}

	// tools/shadow.sh copies an allowlist of root entries; runs/ is not on it,
	// and no tool copies a whole root.
	b, err := os.ReadFile("../../tools/shadow.sh")
	if err != nil {
		t.Fatal(err)
	}
	list := regexp.MustCompile(`(?m)^\s*for x in ([^;]+); do`).FindSubmatch(b)
	if list == nil || strings.Contains(" "+string(list[1])+" ", " runs ") {
		t.Errorf("shadow.sh copy list: %q", list)
	}
	tools, _ := filepath.Glob("../../tools/*.sh")
	whole := regexp.MustCompile(`(cp|rsync|ditto)[^\n]*"\$(src|root|LOCALBASE_ROOT)"/?["\s]`)
	for _, p := range tools {
		s, _ := os.ReadFile(p)
		if m := whole.Find(s); m != nil {
			t.Errorf("%s copies a whole root: %s", p, m)
		}
	}
}
