package web

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// Vendored files must match the hashes recorded in static/vendor/PINS.
func TestVendorPins(t *testing.T) {
	pins, err := staticFS.ReadFile("static/vendor/PINS")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, line := range strings.Split(string(pins), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || strings.HasPrefix(f[0], "#") {
			continue
		}
		b, err := staticFS.ReadFile("static/vendor/" + f[0])
		if err != nil {
			t.Fatalf("%s: %v", f[0], err)
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != f[1] {
			t.Errorf("%s does not match its pinned sha256", f[0])
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no pins checked")
	}
}

// Templates parse and every page defines its main block.
func TestTemplatesParse(t *testing.T) {
	u := New(Deps{})
	for name, tpl := range u.pages {
		if tpl.Lookup("main") == nil {
			t.Errorf("%s has no main block", name)
		}
	}
}
