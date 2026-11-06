package web

import (
	"net/http"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

// The page says what a person can do. Nothing checks configuration:read or configuration:manage, so
// holding them lets a person do nothing, and the page doesn't say otherwise.
func TestSettingsListsOnlyPermissionsThatDoSomething(t *testing.T) {
	p := &auth.Principal{Kind: "browser-session", ID: "u", Name: "Someone", Permissions: auth.Permissions}
	u := New(Deps{SignInEnabled: true, Principal: func(*http.Request) *auth.Principal { return p }, Call: (&fakeTokenAPI{}).call})
	body := get(u, "/app/settings").Body.String()
	if !strings.Contains(body, "Manage models") {
		t.Errorf("it doesn't say what they can do: %.300s", body)
	}
	for _, nothing := range []string{"See configuration", "Change configuration"} {
		if strings.Contains(body, nothing) {
			t.Errorf("it says %q", nothing)
		}
	}
}
