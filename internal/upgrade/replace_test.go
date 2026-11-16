package upgrade

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckReplaceable(t *testing.T) {
	if err := CheckReplaceable("/opt/homebrew/Cellar/local-base/1/bin/local-base"); err == nil || !strings.Contains(err.Error(), "brew upgrade") {
		t.Errorf("Homebrew: %v", err)
	}
	dir := t.TempDir()
	if err := CheckReplaceable(filepath.Join(dir, "local-base")); err != nil {
		t.Errorf("a writable directory: %v", err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := CheckReplaceable(filepath.Join(locked, "local-base")); err == nil || !strings.Contains(err.Error(), "Cannot write") {
		t.Errorf("a read-only directory: %v", err)
	}
}
