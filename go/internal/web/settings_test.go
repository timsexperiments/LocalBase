package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

// fakeTokenAPI answers /_localbase/hf-token like the gateway and records
// what the page sent.
type fakeTokenAPI struct {
	source string
	sent   []map[string]any
	fail   string
}

func (f *fakeTokenAPI) call(_ context.Context, p *auth.Principal, method, path, _ string, body []byte) APIResult {
	if path != "/_localbase/hf-token" || !p.Can("models:manage") {
		return APIResult{Status: 403, Body: []byte(`{"error":{"message":"forbidden"}}`)}
	}
	status := func(extra string) APIResult {
		src := "null"
		if f.source != "" {
			src = `"` + f.source + `"`
		}
		return APIResult{Status: 200, Body: []byte(`{"configured":` + map[bool]string{true: "true", false: "false"}[f.source != ""] + `,"source":` + src + `,"envAvailable":false` + extra + `}`)}
	}
	if method == "GET" {
		return status("")
	}
	var in map[string]any
	json.Unmarshal(body, &in)
	f.sent = append(f.sent, in)
	if f.fail != "" {
		return APIResult{Status: 400, Body: []byte(`{"error":{"message":"` + f.fail + `"}}`)}
	}
	switch in["action"] {
	case "save":
		f.source = "saved"
		return status(`,"check":{"state":"valid","name":"TimsExperiments","role":"read"}`)
	case "clear":
		f.source = ""
		return status("")
	}
	return status(`,"check":{"state":"unreachable","message":"Hugging Face could not be reached."}`)
}

func tokenPost(u *UI, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/app/settings/hf-token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("X-Localbase-Ui", "1")
	withCSRF(u, req)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	u.ServeHTTP(w, req)
	return w
}

// Managers see the token card and can save, check and clear through the
// API; the token is never echoed back. Others don't get the card or the route.
func TestSettingsHFToken(t *testing.T) {
	p := &auth.Principal{Kind: "browser-session", ID: "u", Name: "TimsExperiments", Permissions: []string{"models:read", "inference:chat", "models:manage"}}
	api := &fakeTokenAPI{}
	u := New(Deps{SignInEnabled: true, Principal: func(*http.Request) *auth.Principal { return p }, Call: api.call})
	body := get(u, "/app/settings").Body.String()
	for _, want := range []string{`id="hf-token"`, "Not set", "Signed in as", "TimsExperiments", "Chat · See models · Manage models", `hx-post="/app/settings/hf-token"`} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page: missing %q", want)
		}
	}
	const secret = "hf_abcdefghijklmnop"
	w := tokenPost(u, url.Values{"action": {"save"}, "token": {"  " + secret + " "}})
	out := w.Body.String()
	if w.Code != 200 || !strings.Contains(out, "Hugging Face accepted the token for TimsExperiments (read access).") || !strings.Contains(out, "Clear saved token") {
		t.Fatalf("save: %d %s", w.Code, out)
	}
	if strings.Contains(out, secret) || strings.Contains(out, "<html") {
		t.Error("save echoed the token or rendered a whole page")
	}
	if got := api.sent[0]; got["action"] != "save" || got["token"] != secret || len(got) != 2 {
		t.Errorf("save sent %v", got)
	}
	if out := tokenPost(u, url.Values{"action": {"validate"}}).Body.String(); !strings.Contains(out, "Hugging Face could not be reached.") {
		t.Errorf("check: %s", out)
	}
	if out := tokenPost(u, url.Values{"action": {"clear"}}).Body.String(); !strings.Contains(out, "Cleared the saved token.") || !strings.Contains(out, "Not set") {
		t.Errorf("clear: %s", out)
	}
	if out := tokenPost(u, url.Values{"action": {"save"}, "token": {" "}}).Body.String(); !strings.Contains(out, "Enter a token to save.") {
		t.Errorf("blank token: %s", out)
	}
	api.fail = "Hugging Face rejected that token, so it was not saved."
	if out := tokenPost(u, url.Values{"action": {"save"}, "token": {secret}}).Body.String(); !strings.Contains(out, `role="alert">Hugging Face rejected that token`) {
		t.Errorf("rejected: %s", out)
	}
	if w := tokenPost(u, url.Values{"action": {"drop"}}); w.Code != 400 {
		t.Errorf("unknown action: %d", w.Code)
	}

	p.Permissions = []string{"models:read", "inference:chat"}
	if body := get(u, "/app/settings").Body.String(); strings.Contains(body, `id="hf-token"`) {
		t.Error("the token card shows without models:manage")
	}
	if w := tokenPost(u, url.Values{"action": {"clear"}}); w.Code != http.StatusForbidden {
		t.Errorf("without models:manage: %d", w.Code)
	}
}

// Settings are grouped lists, one setting per row: the history switch is a
// real checkbox (role and state for assistive tech), and the groups are
// Chat, Default chat model, Hugging Face and Account.
func TestSettingsGrouped(t *testing.T) {
	main := mainRe.FindString(get(shellUI(t, nil, ""), "/app/settings").Body.String())
	for _, want := range []string{`<h1 id="settings-title" class="page-title">Settings</h1>`, `<h2 id="history-title" class="list-label">Chat</h2>`,
		`<input type="checkbox" class="switch" data-history-toggle>`, `data-history-clear>Clear</button>`, `href="/app/chat" hx-boost="false" class="setting-row"`} {
		if !strings.Contains(main, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(main, `class="card`) {
		t.Error("settings still use cards")
	}
}

// The default chat models save on change (no Save button that can sit
// under the tab bar), and the fallback order is said in plain words.
func TestDefaultModelsAutoSave(t *testing.T) {
	main := mainRe.FindString(get(shellUI(t, nil, ""), "/app/settings").Body.String())
	if !strings.Contains(main, `id="default-models"`) {
		t.Skip("no default-models group in this setup")
	}
	section := regexp.MustCompile(`(?s)<section id="default-models".*?</section>`).FindString(main)
	if !strings.Contains(section, `hx-post="/app/settings/default-models" hx-trigger="change"`) || strings.Contains(section, `>Save</button>`) ||
		!strings.Contains(section, "If it needs more memory than is free right now, it uses the next one") {
		t.Errorf("default models: %s", section)
	}
}
