package hub

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A planted link at a cache file logs one warning: path and reason, not the body.
func TestCacheWriteRefusalIsLogged(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".localbase-root.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	c := &Client{Root: root, Log: slog.New(slog.NewTextHandler(&logs, nil))}
	const u = "https://huggingface.co/api/models/x"
	os.MkdirAll(filepath.Dir(c.cachePath(u)), 0o700)
	target := filepath.Join(t.TempDir(), "victim")
	os.WriteFile(target, []byte("mine"), 0o644)
	os.Symlink(target, c.cachePath(u))
	c.writeCache(u, []byte("private body"), time.Now())
	got := logs.String()
	if strings.Count(got, "root.write-refused") != 1 || !strings.Contains(got, "symbolic link") || strings.Contains(got, "private body") {
		t.Errorf("%s", got)
	}
	if b, _ := os.ReadFile(target); string(b) != "mine" {
		t.Errorf("the target: %q", b)
	}
}
