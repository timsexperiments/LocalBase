package auth

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

// Each Browser's sign-in pages link its own style sheet. Two with different ones, rendering at the
// same time, never see each other's: there's no package-wide style sheet to share. One that has none
// links the unversioned sheet.
func TestEachBrowserLinksItsOwnStylesheet(t *testing.T) {
	for _, css := range []string{"/app/assets/app.css?v=aaaaaaaaaaaa", "/app/assets/app.css?v=bbbbbbbbbbbb", ""} {
		t.Run("stylesheet="+css, func(t *testing.T) {
			t.Parallel()
			b := NewBrowser(nil, t.TempDir(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
			b.Stylesheet = css
			want := css
			if want == "" {
				want = "/app/assets/app.css"
			}
			for range 50 {
				w := httptest.NewRecorder()
				b.MagicLinkPage(w)
				if link := `<link rel="stylesheet" href="` + want + `">`; !strings.Contains(w.Body.String(), link) {
					t.Fatalf("want %s in\n%.300s", link, w.Body.String())
				}
			}
		})
	}
}
