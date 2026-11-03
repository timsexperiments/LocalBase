package root

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A root the v0.1.0 installer left, with only licenses/ in it, can be initialized and
// keeps them. Anything else in the directory is still refused.
func TestInitToleratesOnlyLicenses(t *testing.T) {
	dir := t.TempDir()
	license := filepath.Join(dir, "licenses", "LICENSE")
	os.MkdirAll(filepath.Dir(license), 0o755)
	os.WriteFile(license, []byte("terms"), 0o644)
	r, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(r.Dir); err != nil {
		t.Errorf("not initialized: %v", err)
	}
	if got, _ := os.ReadFile(license); string(got) != "terms" {
		t.Errorf("the licenses: %q", got)
	}
	if again, err := Init(dir); err != nil || again.Dir != r.Dir {
		t.Errorf("a second init: %v", err)
	}

	refused := map[string]func(dir string){
		"another file beside it": func(d string) {
			os.Mkdir(filepath.Join(d, "licenses"), 0o755)
			os.WriteFile(filepath.Join(d, "notes.txt"), nil, 0o644)
		},
		"another directory": func(d string) { os.Mkdir(filepath.Join(d, "photos"), 0o755) },
		"a file called licenses": func(d string) {
			os.WriteFile(filepath.Join(d, "licenses"), nil, 0o644)
		},
		"a link called licenses": func(d string) {
			os.Symlink(t.TempDir(), filepath.Join(d, "licenses"))
		},
	}
	for name, plant := range refused {
		d := t.TempDir()
		plant(d)
		if _, err := Init(d); err == nil || !strings.Contains(err.Error(), "nonempty") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
