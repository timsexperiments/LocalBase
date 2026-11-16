package web

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// A module that another script imports by relative path ("./person.js") is fetched without the
// page's ?v= fingerprint, so it must never be cached as immutable: a release that changes it
// would otherwise run beside the old copy until a hard reload. Each such module exists, and is
// served for revalidation every time.
func TestRelativeImportsAreNeverCachedStale(t *testing.T) {
	u := New(Deps{NoAuth: true})
	entries, err := staticFS.ReadDir("static/js")
	if err != nil {
		t.Fatal(err)
	}
	spec := regexp.MustCompile(`(?:from|import\()\s*"\./([a-z0-9-]+\.js)"`)
	seen := map[string]string{}
	for _, e := range entries {
		b, err := staticFS.ReadFile("static/js/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range spec.FindAllStringSubmatch(string(b), -1) {
			seen[m[1]] = e.Name()
		}
	}
	if len(seen) == 0 {
		t.Fatal("no relative imports found: the pattern no longer matches how scripts import each other")
	}
	for name, from := range seen {
		w := httptest.NewRecorder()
		u.ServeHTTP(w, httptest.NewRequest("GET", "/app/assets/js/"+name, nil))
		cc := w.Header().Get("Cache-Control")
		if w.Code != 200 {
			t.Errorf("%s imports ./%s, which isn't served: %d", from, name, w.Code)
		} else if cc != "no-cache" || strings.Contains(cc, "immutable") {
			t.Errorf("./%s (imported by %s) is fetched without a version but cached as %q", name, from, cc)
		}
	}
}
