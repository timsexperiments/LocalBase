package web

import (
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

func get(u *UI, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	u.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

func TestAppOpensChat(t *testing.T) {
	u := New(Deps{NoAuth: true})
	for _, p := range []string{"/", "/app"} {
		if w := get(u, p); w.Code != http.StatusFound || w.Header().Get("Location") != "/app/chat" {
			t.Errorf("%s: %d %s", p, w.Code, w.Header().Get("Location"))
		}
	}
}

// Without a ready chat model the page says so, links to the Library, and
// the composer is off; the page still has its log, status region and
// modules.
func TestChatWithoutReadyModel(t *testing.T) {
	w := get(New(Deps{NoAuth: true}), "/app/chat")
	body := w.Body.String()
	for _, want := range []string{`role="log"`, `aria-live="off"`, `id="chat-status" role="status"`, "No chat model is ready",
		`href="/app/models"`, `id="chat-models">[]</script>`, "js/chat.js", "interactive-widget=resizes-content", `hx-boost="false"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if !strings.Contains(body, `id="chat-input" rows="1"`) || !strings.Contains(body, "Message\" autocomplete=\"off\" disabled") {
		t.Error("the composer is enabled without a ready model")
	}
	if strings.Contains(body, "maximum-scale") || strings.Contains(body, "user-scalable") {
		t.Error("the page disables zoom")
	}
}

// A person who may read models but not chat is told why.
func TestChatWithoutPermission(t *testing.T) {
	p := &auth.Principal{Kind: "browser-session", ID: "u", Permissions: []string{"models:read"}}
	u := New(Deps{SignInEnabled: true, Principal: func(*http.Request) *auth.Principal { return p }})
	w := get(u, "/app/chat")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "This account can’t chat") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// Without models:read the page is forbidden, as the other pages are.
	p.Permissions = []string{"inference:chat"}
	if w := get(u, "/app/chat"); w.Code != http.StatusForbidden {
		t.Fatalf("without models:read: %d", w.Code)
	}
}

// Model names are data in a JSON block: they cannot end the script.
func TestChatModelsJSONIsInert(t *testing.T) {
	b, _ := json.Marshal([]chatModel{{ID: "x", Name: "</script><script>alert(1)</script>"}})
	tpl := template.Must(template.New("t").Parse(`<script type="application/json">{{.}}</script>`))
	var out strings.Builder
	tpl.Execute(&out, template.JS(b))
	if strings.Count(out.String(), "</script>") != 1 {
		t.Fatalf("escaped JSON: %s", out.String())
	}
}

func TestSettingsPage(t *testing.T) {
	w := get(New(Deps{NoAuth: true}), "/app/settings")
	body := w.Body.String()
	for _, want := range []string{"Save text history on this device", "Clear all history", "js/settings.js", `aria-current="page"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
}
