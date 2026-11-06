package root

import (
	"os"
	"path/filepath"
	"testing"
)

// A message names the default root in words, so someone reading it knows it's the one their installed
// LocalBase uses. However the path is spelled, the answer has to be the same.
func TestIsDefault(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("LOCALBASE_ROOT", filepath.Join(home, "chosen"))
	def := filepath.Join(home, ".local", "share", "local-base")
	if err := os.MkdirAll(def, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "link")
	if err := os.Symlink(def, link); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{
		def:                                    true,
		def + "/":                              true,
		filepath.Join(def, "..", "local-base"): true,
		link:                                   true,
		filepath.Join(home, "chosen"):          false, // what LOCALBASE_ROOT names isn't the default
		filepath.Join(def, "inner"):            false,
		filepath.Dir(def):                      false,
		"/":                                    false,
		"":                                     false,
	} {
		if got := IsDefault(path); got != want {
			t.Errorf("IsDefault(%q) = %v, want %v", path, got, want)
		}
	}
	const note = " This is the default root, the one your installed LocalBase uses."
	if got := DefaultRootNote(def); got != note {
		t.Errorf("the note on the default root: %q", got)
	}
	if got := DefaultRootNote(filepath.Join(home, "chosen")); got != "" {
		t.Errorf("the note on another root: %q", got)
	}
}

// With no home to go by there's no default root, so nothing can be it.
func TestIsDefaultWithNoHome(t *testing.T) {
	t.Setenv("HOME", "")
	if IsDefault("/") || IsDefault("/.local/share/local-base") {
		t.Error("with no home, nothing is the default root")
	}
}
