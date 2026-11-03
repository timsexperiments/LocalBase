package service

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/obs"
)

// launchd opens the stderr path itself, so preparing it must not create or touch
// anything through a planted link.
func TestPrepareStderrRefusesALink(t *testing.T) {
	root := t.TempDir()
	logs := filepath.Join(root, "logs")
	os.MkdirAll(logs, 0o700)
	target := filepath.Join(t.TempDir(), "victim")
	os.WriteFile(target, []byte("mine"), 0o644)
	os.Symlink(target, filepath.Join(logs, "service-stderr.log"))
	d := Definition{Metadata: Metadata{Manager: "launchd", DefinitionPath: filepath.Join(t.TempDir(), "unit.plist")}, Contents: "x"}
	err := d.write(root)
	var unsafe *obs.UnsafeFileError
	if !errors.As(err, &unsafe) {
		t.Fatalf("want a refusal: %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "mine" {
		t.Errorf("the target: %q", got)
	}
	if _, err := os.Stat(d.DefinitionPath); err == nil {
		t.Error("the unit was written anyway")
	}

	// A normal root gets the private file.
	root = t.TempDir()
	if err := d.write(root); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, "logs", "service-stderr.log"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("%v %v", info, err)
	}
}
