package catalog

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A planted link at a catalog.d entry being adopted logs one warning, and the file
// it points at is not created.
func TestAdoptionRefusalIsLogged(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	root := t.TempDir()
	path, err := UserEntryPath(root, selectedFormer)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(path), 0o700)
	target := filepath.Join(t.TempDir(), "not-yet")
	os.Symlink(target, path)
	if got := AdoptFormerEntries(root, []string{selectedFormer}); len(got) != 0 {
		t.Errorf("adopted: %v", got)
	}
	if out := logs.String(); strings.Count(out, "root.write-refused") != 1 || !strings.Contains(out, "symbolic link") {
		t.Errorf("%s", out)
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("created the link's target")
	}
}
