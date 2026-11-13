package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A bundle is how a broken root gets reported, so a root that can't be opened still gets one, and it
// says the database was unreadable. The reason goes only to the person running it: bundles get shared,
// and the reason holds a path.
func TestDiagnosticsOnARefusedRootSaysSoAndKeepsTheReasonOut(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("LOCALBASE_ROOT", "")
	root := filepath.Join(t.TempDir(), "private-root-name")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	// A root that stopped cleanly has no -wal of its own, so this one is refused.
	if err := os.WriteFile(filepath.Join(root, "local-base.db-wal"), bytes.Repeat([]byte{7}, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "bundle.zip")
	var out, errb bytes.Buffer
	a := app()
	a.Stdout, a.Stderr = &out, &errb
	if code := a.Run([]string{"--root", root, "diagnostics", "--output", bundle}); code != 0 {
		t.Fatalf("exit %d: %s%s", code, out.String(), errb.String())
	}
	const reason = "holds writes that may belong"
	if !strings.Contains(errb.String(), "Could not read this root's database: ") || !strings.Contains(errb.String(), reason) {
		t.Errorf("stderr doesn't say why: %q", errb.String())
	}
	if strings.Contains(out.String(), reason) {
		t.Errorf("the reason reached stdout: %q", out.String())
	}

	archive, err := zip.OpenReader(bundle)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	var seenManifest bool
	for _, f := range archive.File {
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{reason, "private-root-name", "-wal-owner", "Could not read this root"} {
			if bytes.Contains(body, []byte(secret)) {
				t.Errorf("%s holds %q:\n%.400s", f.Name, secret, body)
			}
		}
		if f.Name != "manifest.json" {
			continue
		}
		seenManifest = true
		var manifest struct {
			Database struct {
				Available *bool  `json:"available"`
				Error     string `json:"error"`
			} `json:"database"`
		}
		if err := json.Unmarshal(body, &manifest); err != nil {
			t.Fatal(err)
		}
		if manifest.Database.Available == nil || *manifest.Database.Available || manifest.Database.Error != "database_unavailable" {
			t.Errorf("manifest's database: %+v", manifest.Database)
		}
	}
	if !seenManifest {
		t.Error("no manifest.json in the bundle")
	}
}
