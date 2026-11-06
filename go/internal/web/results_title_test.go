package web

import (
	"regexp"
	"testing"
)

// A tool's page is a region named by its title, and its results are a region named
// by their hidden heading. Two regions with one name tell a screen reader nothing,
// and axe says so (landmark-unique): Speech's results were "Speech" on the Speech page.
func TestResultsAreNamedApartFromTheirPage(t *testing.T) {
	html, err := templateFS.ReadFile("templates/lab.html")
	if err != nil {
		t.Fatal(err)
	}
	headings := regexp.MustCompile(`eq \.Tab "(\w+)"\}\}<h2 id="results-title" class="sr-only">([^<]+)</h2>`).FindAllStringSubmatch(string(html), -1)
	if len(headings) < 3 {
		t.Fatalf("found %d hidden results headings, so this checked too little", len(headings))
	}
	for _, h := range headings {
		tab, name := h[1], h[2]
		page, ok := studioNames[tab]
		if !ok {
			t.Errorf("results heading for %q, which isn't a Studio tool", tab)
			continue
		}
		if name == page {
			t.Errorf("%s: its results are named %q, the same as the page", tab, name)
		}
	}
}
