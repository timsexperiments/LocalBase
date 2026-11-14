package web

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

// auth's sign-in pages link the app's own style sheet, with the version the UI's pages use, so they
// look like the rest of the app and pick up a new style sheet as soon as it ships.
func TestAuthSignInPagesLinkTheUIsStylesheet(t *testing.T) {
	u := New(Deps{NoAuth: true})
	b := auth.NewBrowser(nil, t.TempDir(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w := httptest.NewRecorder()
	b.MagicLinkPage(w)
	want := `<link rel="stylesheet" href="/app/assets/app.css?v=` + u.assetVer["app.css"] + `">`
	if u.assetVer["app.css"] == "" || !strings.Contains(w.Body.String(), want) {
		t.Errorf("the magic-link page's style sheet: want %s in\n%.400s", want, w.Body.String())
	}
}
