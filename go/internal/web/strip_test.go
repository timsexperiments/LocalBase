package web

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

// When the gateway can't read this computer, the strip's refresh says so, as Running does: the
// same strip with no reading. It used to draw the strip from nothing, which is a template error.
func TestStripRefreshWhenTheHostCantBeRead(t *testing.T) {
	t.Setenv("LOCALBASE_COMPUTER_NAME", "Mac Studio")
	api := func(_ context.Context, _ *auth.Principal, _, _, _ string, _ []byte) APIResult {
		return APIResult{Status: 503, Body: []byte(`{"error":{"message":"not now"}}`)}
	}
	var logs bytes.Buffer
	p := &auth.Principal{Kind: "browser-session", ID: "u", Permissions: []string{"models:read"}}
	u := New(Deps{SignInEnabled: true, Principal: func(*http.Request) *auth.Principal { return p }, RootDir: t.TempDir(), Call: api,
		Log: slog.New(slog.NewTextHandler(&logs, nil))})
	w := httptest.NewRecorder()
	u.ServeHTTP(w, httptest.NewRequest("GET", "/app/strip", nil))
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `id="memory-strip"`) || !strings.Contains(body, "Memory reading unavailable") || !strings.Contains(body, "Mac Studio") {
		t.Errorf("%d %q", w.Code, body)
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("logged: %s", logs.String())
	}
}
