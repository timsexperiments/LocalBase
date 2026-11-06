package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A browser can put typed words back into a page it loads again, for whoever is signed in now, and
// only autocomplete stops it: back/forward handling doesn't reach this. So every typed box says no.
// lab.html is held by TestStudioBoxesStayOutOfFormRestore.
func TestTypedBoxesStayOutOfFormRestore(t *testing.T) {
	tag := regexp.MustCompile(`<(input|textarea)\b[^>]*>`)
	notTyped := regexp.MustCompile(`type="?(hidden|checkbox|radio|file|submit|button|range|color)"?`)
	name := regexp.MustCompile(`(?:id|name)="([^"]*)"`)
	// Credentials, where a password manager needs the right token. Browsers don't restore passwords.
	credentials := map[string]string{"client-secret": "new-password", "smtp-user": "username", "smtp-pass": "new-password"}
	files, err := filepath.Glob("templates/*.html")
	if err != nil || len(files) == 0 {
		t.Fatalf("no templates: %v", err)
	}
	boxes := 0
	for _, f := range files {
		if filepath.Base(f) == "lab.html" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, box := range tag.FindAllString(string(b), -1) {
			if notTyped.MatchString(box) {
				continue
			}
			boxes++
			id := ""
			if m := name.FindStringSubmatch(box); m != nil {
				id = m[1]
			}
			if want, ok := credentials[id]; ok {
				if !strings.Contains(box, `autocomplete="`+want+`"`) {
					t.Errorf("%s: %s should have autocomplete=%q: %s", filepath.Base(f), id, want, box)
				}
				continue
			}
			if !strings.Contains(box, `autocomplete="off"`) {
				t.Errorf("%s: a typed box without autocomplete=\"off\": %s", filepath.Base(f), box)
			}
		}
	}
	if boxes < 30 {
		t.Errorf("only %d boxes found: is the pattern still finding them?", boxes)
	}
}
