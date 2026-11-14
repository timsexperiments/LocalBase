package web

import (
	"html"
	"io"
	"log/slog"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

// auth's sign-in pages link the app's own style sheet, with the version the UI's pages use, so they
// look like the rest of the app and pick up a new style sheet as soon as it ships.
func TestAuthSignInPagesLinkTheUIsStylesheet(t *testing.T) {
	u := New(Deps{NoAuth: true})
	b := auth.NewBrowser(nil, t.TempDir(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.Stylesheet = StylesheetURL()
	w := httptest.NewRecorder()
	b.MagicLinkPage(w)
	want := `<link rel="stylesheet" href="/app/assets/app.css?v=` + u.assetVer["app.css"] + `">`
	if u.assetVer["app.css"] == "" || !strings.Contains(w.Body.String(), want) {
		t.Errorf("the magic-link page's style sheet: want %s in\n%.400s", want, w.Body.String())
	}
}

// StylesheetURL is the style sheet the UI's own pages link: one version, from one place. /app sends
// a signed-in person here, and every page shares its layout.
func TestStylesheetURLIsWhatTheUIsPagesLink(t *testing.T) {
	w := get(New(Deps{NoAuth: true}), "/app/chat")
	m := regexp.MustCompile(`<link rel="stylesheet" href="([^"]+)">`).FindStringSubmatch(w.Body.String())
	if m == nil {
		t.Fatalf("no style sheet link on the UI's page (%d):\n%.400s", w.Code, w.Body.String())
	}
	if got := html.UnescapeString(m[1]); got != StylesheetURL() || !strings.Contains(got, "?v=") {
		t.Errorf("the UI's pages link %s; StylesheetURL() is %s", got, StylesheetURL())
	}
}
