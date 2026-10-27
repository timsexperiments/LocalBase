package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// shellUI renders every page: a scratch store, the built-in catalog, an
// idle LLM runtime and the host memory reader. p nil means --no-auth.
func shellUI(t *testing.T, p *auth.Principal, logoutURL string) *UI {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cat, _ := catalog.Load("")
	llm := runtime.NewLLM(runtime.LLMDeps{Catalog: cat, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, &store.Config{})
	call := func(context.Context, *auth.Principal, string, string, string, []byte) APIResult {
		return APIResult{Status: 200, Body: []byte(`{}`)}
	}
	d := Deps{NoAuth: p == nil, Call: call, Catalog: cat, Store: st, LLM: llm, RootDir: dir, LogoutURL: logoutURL,
		Memory: memory.NewController(memory.NewHostProvider(), memory.Reserve{Percent: 15, MinimumGB: 8}, false)}
	if p != nil {
		d.SignInEnabled = true
		d.Principal = func(*http.Request) *auth.Principal { return p }
	}
	return New(d)
}

// Pages map to the destination the tab bar and sidebar mark as current.
func TestSection(t *testing.T) {
	for page, want := range map[string]string{
		"chat": "chat", "library": "models", "get": "models", "details": "models", "import": "models", "backends": "models",
		"lab": "lab", "running": "running", "voices": "more", "settings": "more", "admin": "more",
	} {
		if got := section(page); got != want {
			t.Errorf("section(%q) = %q, want %q", page, got, want)
		}
	}
}

var tabRe = regexp.MustCompile(`(?s)<nav class="tabbar" aria-label="Primary">(.*?)</nav>`)

// Every signed-in page has the phone tab bar (four destinations and More),
// the desktop sidebar and the More sheet, and marks exactly one tab current.
// Account actions are not in a header row.
func TestShellOnEveryPage(t *testing.T) {
	u := shellUI(t, nil, "")
	for path, current := range map[string]string{
		"/app/chat": "Chat", "/app/models": "Models", "/app/models/get": "Models", "/app/models/running": "Running",
		"/app/lab": "Lab", "/app/voices": "", "/app/settings": "", "/app/admin": "",
	} {
		w := get(u, path)
		body := w.Body.String()
		if w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
		m := tabRe.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("%s: no tab bar", path)
		}
		tabs := m[1]
		for _, label := range []string{">Chat<", ">Models<", ">Lab<", ">Running<", ">More"} {
			if !strings.Contains(tabs, label) {
				t.Errorf("%s: tab bar lacks %s", path, label)
			}
		}
		if n := strings.Count(tabs, `aria-current="page"`); (current == "" && n != 0) || (current != "" && n != 1) {
			t.Errorf("%s: %d current tabs", path, n)
		}
		if current != "" && !regexp.MustCompile(`aria-current="page">.*?<span class="tab-label">`+current+`<`).MatchString(tabs) {
			t.Errorf("%s: %s is not the current tab", path, current)
		}
		if current == "" && !strings.Contains(tabs, `data-active>`) {
			t.Errorf("%s: More is not marked active", path)
		}
		for _, want := range []string{`<aside class="sidebar"`, `<dialog id="more-sheet" class="sheet"`, `data-open-sheet="more-sheet" aria-haspopup="dialog"`,
			`popovertarget="account-menu"`, "js/shell.js", `data-run-pill="sidebar"`, `data-run-pill="dock"`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: missing %q", path, want)
			}
		}
		if strings.Contains(body, `aria-label="Sections"`) {
			t.Errorf("%s: still has the old section grid", path)
		}
	}
}

// Every page lets Chrome on Android resize for the keyboard and never
// disables zoom.
func TestViewportKeyboardAndZoom(t *testing.T) {
	u := shellUI(t, nil, "")
	for _, path := range []string{"/app/chat", "/app/models", "/app/lab", "/app/voices", "/app/settings", "/app/admin"} {
		body := get(u, path).Body.String()
		if !strings.Contains(body, "interactive-widget=resizes-content") {
			t.Errorf("%s: no interactive-widget=resizes-content", path)
		}
		if strings.Contains(body, "maximum-scale") || strings.Contains(body, "user-scalable") {
			t.Errorf("%s: disables zoom", path)
		}
	}
	// Docked composers opt out of the shell's scroll-into-view.
	if !strings.Contains(get(u, "/app/chat").Body.String(), `id="composer" aria-label="Message composer" data-docked`) {
		t.Error("the chat composer is not marked docked")
	}
}

// Admin appears only with an admin permission; Sign out sits in the account
// menu and the More sheet when sign-in is on, and nowhere without it.
func TestShellAccountAndAdmin(t *testing.T) {
	p := &auth.Principal{Kind: "browser-session", ID: "u", Name: "Émile", Permissions: []string{"models:read"}}
	u := shellUI(t, p, "https://edge.example/logout")
	body := get(u, "/app/models").Body.String()
	if strings.Contains(body, `href="/app/admin"`) {
		t.Error("Admin shown without an admin permission")
	}
	if n := strings.Count(body, `data-logout data-logout-url="https://edge.example/logout"`); n != 2 {
		t.Errorf("%d sign-out buttons, want the account menu and the More sheet", n)
	}
	if !strings.Contains(body, `<span aria-hidden="true" class="avatar">É</span>`) || !strings.Contains(body, "Signed in as <span") {
		t.Error("the account shows no initial or name")
	}
	p.Permissions = append(p.Permissions, "keys:read")
	if n := strings.Count(get(u, "/app/models").Body.String(), `href="/app/admin"`); n != 2 {
		t.Errorf("Admin linked %d times, want the sidebar and the More sheet", n)
	}
	if strings.Contains(get(shellUI(t, nil, ""), "/app/models").Body.String(), "data-logout") {
		t.Error("Sign out offered without sign-in")
	}
}

// Library and Get models share a Models switch; other pages do not show it.
func TestModelsSwitch(t *testing.T) {
	u := shellUI(t, nil, "")
	for path, current := range map[string]string{"/app/models": "Library", "/app/models/get": "Get models"} {
		body := get(u, path).Body.String()
		if !regexp.MustCompile(`<nav aria-label="Models" class="segmented[^"]*">[\s\S]*?aria-current="page">` + current + `</a>`).MatchString(body) {
			t.Errorf("%s: the Models switch does not mark %s", path, current)
		}
	}
	if strings.Contains(get(u, "/app/lab").Body.String(), `aria-label="Models"`) {
		t.Error("the Models switch shows on the Lab")
	}
}

// Without a signed-in person there is no navigation to show.
func TestShellSignedOut(t *testing.T) {
	u := New(Deps{})
	body := get(u, "/app/models").Body.String()
	if strings.Contains(body, `class="tabbar"`) || strings.Contains(body, `class="sidebar"`) || !strings.Contains(body, "Sign-in isn") {
		t.Errorf("signed-out page: %s", body)
	}
}

// On phones the model picker and generation controls open as bottom sheets;
// the header and composer use the quiet round icon buttons.
func TestChatSheets(t *testing.T) {
	body := get(shellUI(t, nil, ""), "/app/chat").Body.String()
	for _, want := range []string{`<dialog id="model-picker" aria-labelledby="picker-title" class="sheet`,
		`<dialog id="generation" aria-labelledby="generation-title" class="sheet`,
		`class="icon-btn lg:hidden" data-open-drawer`, `aria-label="Open conversations"`, `data-stop hidden aria-label="Stop generation"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// Running now: memory first, then what is in memory; the polled region
// carries one sentence that shell.js announces only when it changes, into
// a status region outside the swapped part.
func TestRunningNow(t *testing.T) {
	main := mainRe.FindString(get(shellUI(t, nil, ""), "/app/models/running").Body.String())
	for _, want := range []string{`<h1 id="running-title" class="page-title">Running now</h1>`, `id="memory-title"`,
		`data-announce="Nothing is loaded." data-announce-into="running-status"`,
		`<p id="running-status" class="sr-only" role="status" aria-live="polite"></p>`, "Nothing loaded"} {
		if !strings.Contains(main, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Index(main, `id="running-status"`) > strings.Index(main, `id="running-live"`) {
		t.Error("the status region is inside the polled part")
	}
}

// Unload: the outcomes of the gateway's unload route, in words.
func TestUnload(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		want   string
	}{
		{200, `{"unloaded":true,"memoryFreedBytes":4200000000}`, "Unloaded. Freed 4.2 GB."},
		{409, `{"error":{"code":"model_busy","in_flight":2,"message":"busy"}}`, "Still busy with 2 requests, so it stayed loaded."},
		{404, `{"error":{"code":"model_not_loaded","message":"x"}}`, "It was already unloaded."},
		{404, `404 page not found`, "This LocalBase can’t unload models yet."},
		{403, `{"error":{"code":"forbidden","message":"Missing models:manage."}}`, "Missing models:manage."},
	} {
		u := shellUI(t, nil, "")
		var path string
		u.d.Call = func(_ context.Context, _ *auth.Principal, method, p, _ string, _ []byte) APIResult {
			path = method + " " + p
			return APIResult{Status: c.status, Body: []byte(c.body)}
		}
		w := httptest.NewRecorder()
		req := htmxPost("/app/models/unload/qwen3.5-4b-q4_k_m", url.Values{})
		withCSRF(u, req) // this UI has a root, so its own key
		u.ServeHTTP(w, req)
		body := w.Body.String()
		if path != "POST /_localbase/models/qwen3.5-4b-q4_k_m/unload" || !strings.Contains(body, c.want) ||
			!strings.Contains(body, `data-announce="`+c.want) {
			t.Errorf("%d %s: %s\n%s", c.status, c.body, path, body)
		}
	}
	// Without models:manage there is no Unload button, and the route refuses.
	p := &auth.Principal{Kind: "browser-session", ID: "u", Permissions: []string{"models:read"}}
	u := shellUI(t, p, "")
	if strings.Contains(get(u, "/app/models/running").Body.String(), "/app/models/unload/") {
		t.Error("Unload offered without models:manage")
	}
}
