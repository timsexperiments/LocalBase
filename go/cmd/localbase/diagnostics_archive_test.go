package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// The bundle counts the stored custom voices, and says nothing else about them.
func TestDiagnosticsCountsCustomVoices(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("LOCALBASE_ROOT", "")
	root := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	count := func() any {
		t.Helper()
		bundle := filepath.Join(t.TempDir(), "bundle.zip")
		if code, out := runApp(t, "--root", root, "diagnostics", "--output", bundle); code != 0 {
			t.Fatalf("diagnostics: %s", out)
		}
		zr, err := zip.OpenReader(bundle)
		if err != nil {
			t.Fatal(err)
		}
		defer zr.Close()
		for _, f := range zr.File {
			if f.Name == "manifest.json" {
				rc, _ := f.Open()
				var m struct {
					Voices map[string]any `json:"voices"`
				}
				if err := json.NewDecoder(rc).Decode(&m); err != nil {
					t.Fatal(err)
				}
				rc.Close()
				return m.Voices["data"]
			}
		}
		t.Fatal("no manifest")
		return nil
	}
	if got := fmt.Sprint(count()); got != "map[customVoices:0]" {
		t.Errorf("none: %s", got)
	}
	st, err := store.Open(filepath.Join(root, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if _, err := st.DB.Exec(`INSERT INTO tts_custom_voices (id, name, name_key, sha256, size_bytes, duration_ms, created_at, created_by, consent_statement, consent_attested_at)
			VALUES (?, ?, ?, ?, 1000, 5000, '2026-01-01T00:00:00.000Z', 'cli', 'I consent.', '2026-01-01T00:00:00.000Z')`, "voice_"+id, "Name "+id, "name "+id, strings.Repeat(id, 64)); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	if got := fmt.Sprint(count()); got != "map[customVoices:2]" {
		t.Errorf("two: %s", got)
	}
}
