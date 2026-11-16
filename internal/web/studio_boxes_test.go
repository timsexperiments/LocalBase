package web

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// A browser can put typed words back into a form when a page comes back from
// history, into whoever's page it now is. autocomplete="off" is what stops it: with
// it gone from the prompt, B's box showed what A typed and never sent, on Chrome 114
// and 156 (DESIGN §27.2e).
func TestStudioBoxesStayOutOfFormRestore(t *testing.T) {
	box := regexp.MustCompile(`<(?:textarea|input)\b[^>]*>`)
	notTyped := regexp.MustCompile(`type="(?:hidden|radio|checkbox|file|range)"`)
	seen := 0
	for _, name := range []string{"lab.html", "studio.html", "voices.html"} {
		html, err := templateFS.ReadFile("templates/" + name)
		if err != nil {
			t.Fatal(err)
		}
		for _, tag := range box.FindAllString(string(html), -1) {
			if notTyped.MatchString(tag) {
				continue
			}
			seen++
			if !strings.Contains(tag, `autocomplete="off"`) {
				t.Errorf("%s: a typed box can come back with someone else's words: %s", name, tag)
			}
		}
	}
	if seen < 7 {
		t.Errorf("found %d typed boxes on the Studio pages, so this checked too little", seen)
	}
	// Decisions builds its option boxes in script, and they hold typed words too.
	js, err := os.ReadFile("static/js/decisions.js")
	if err != nil {
		t.Fatal(err)
	}
	built := regexp.MustCompile(`el\("input", \{ class: "input"[^\n]*`).FindAllString(string(js), -1)
	if len(built) < 2 {
		t.Errorf("found %d built boxes in decisions.js, so this checked too little", len(built))
	}
	for _, line := range built {
		if !strings.Contains(line, `autocomplete: "off"`) {
			t.Errorf("decisions.js: a built box can come back with someone else's words: %s", line)
		}
	}
}
