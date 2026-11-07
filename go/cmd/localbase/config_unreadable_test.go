package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A database that is there but can't be looked at is not "no database": config plan and apply
// would otherwise diff against nothing and take the fresh-root path (review 01-M10).
func TestAnUnreadableDatabaseIsNotAMissingOne(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through permissions")
	}
	dir := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "local-base.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o600); err != nil { // no search permission: stat fails with EACCES
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	cfg, _, err := readConfigIfPresent(dir, nil)
	var unreadable *unreadableSettings
	if err == nil || !errors.As(err, &unreadable) || !strings.HasPrefix(err.Error(), "Could not read the settings of "+dir+": ") {
		t.Errorf("readConfigIfPresent: %v, %v; want the settings reported unreadable", cfg, err)
	}
	if _, err := os.Stat(filepath.Join(t.TempDir(), "nothing", "local-base.db")); err == nil {
		t.Fatal("setup")
	}
	if cfg, _, err := readConfigIfPresent(filepath.Join(t.TempDir(), "nothing"), nil); cfg != nil || err != nil {
		t.Errorf("a missing database: %v, %v; want none and no error", cfg, err)
	}
}
