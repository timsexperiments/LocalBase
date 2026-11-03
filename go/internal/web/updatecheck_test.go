package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

type fakeUpdateAPI struct {
	state string
	sent  []map[string]any
}

func (f *fakeUpdateAPI) call(_ context.Context, _ *auth.Principal, method, path, _ string, body []byte) APIResult {
	if path != "/_localbase/update" {
		return APIResult{Status: 200, Body: []byte(`{}`)}
	}
	if method == "POST" {
		var in map[string]any
		json.Unmarshal(body, &in)
		f.sent = append(f.sent, in)
	}
	return APIResult{Status: 200, Body: []byte(f.state)}
}

func updateUI(t *testing.T, api *fakeUpdateAPI, perms ...string) *UI {
	t.Helper()
	base := connectUI(t, person(append([]string{"models:read"}, perms...)...), &fakeKeysAPI{}, "127.0.0.1:2273")
	d := base.d
	d.Call = api.call
	return New(d)
}

func TestUpdateCheckInSettings(t *testing.T) {
	const off = `{"object":"localbase.update","enabled":false,"current":"0.2.0","latest":null,"url":null,"checkedAt":null,"updateAvailable":false}`
	const on = `{"object":"localbase.update","enabled":true,"current":"0.2.0","latest":"v0.3.0","url":"https://example.test/r","checkedAt":"2026-11-02T12:00:00Z","updateAvailable":true}`

	// Someone who can change it, while it's off: it says what turning it on does.
	_, body := connectBody(t, updateUI(t, &fakeUpdateAPI{state: off}, "access:manage"), "/app/settings")
	for _, want := range []string{"Off", "once a day", "sends nothing about this computer", "Turn on", "local-base upgrade --check"} {
		if !strings.Contains(body, want) {
			t.Errorf("off: missing %q", want)
		}
	}
	// A newer version: it says so, with the command, and offers no install button.
	_, body = connectBody(t, updateUI(t, &fakeUpdateAPI{state: on}, "system:manage"), "/app/settings")
	for _, want := range []string{"v0.3.0 is available", "local-base upgrade", "Turn off", `href="https://example.test/r"`} {
		if !strings.Contains(body, want) {
			t.Errorf("on: missing %q", want)
		}
	}
	if strings.Contains(strings.ToLower(body), "install now") {
		t.Error("there is no install button")
	}
	// Someone who cannot change it sees the state, not the button.
	_, body = connectBody(t, updateUI(t, &fakeUpdateAPI{state: off}), "/app/settings")
	if strings.Contains(body, `hx-post="/app/settings/update-check"`) {
		t.Error("offered without the permission")
	}
	if !strings.Contains(body, "system:manage") {
		t.Error("should say what it needs")
	}
}

func TestUpdateCheckToggle(t *testing.T) {
	api := &fakeUpdateAPI{state: `{"object":"localbase.update","enabled":true,"current":"0.2.0","latest":null,"url":null,"checkedAt":null,"updateAvailable":false}`}
	u := updateUI(t, api, "access:manage")
	r := httptest.NewRequest("POST", "/app/settings/update-check", strings.NewReader(url.Values{"enabled": {"on"}}.Encode()))
	r.Host = "127.0.0.1:2273"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("HX-Request", "true")
	r.Header.Set("X-Localbase-Ui", "1")
	withCSRF(u, r)
	w := httptest.NewRecorder()
	u.ServeHTTP(w, r)
	if w.Code != 200 || len(api.sent) != 1 || api.sent[0]["action"] != "set-check" || api.sent[0]["enabled"] != true {
		t.Fatalf("%d, sent %v", w.Code, api.sent)
	}
	if strings.Contains(w.Body.String(), "<html") || !strings.Contains(w.Body.String(), `id="update-check"`) {
		t.Errorf("not the fragment: %.120s", w.Body.String())
	}
}
