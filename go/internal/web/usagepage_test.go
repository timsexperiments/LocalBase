package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

const usageReply = `{"object":"localbase.usage","scope":"self","range":{"from":"2026-10-26T00:00:00Z","to":"2026-11-02T23:00:00Z","step":"hour"},
 "recording":true,"retentionDays":30,
 "totals":{"requests":1240,"failed":3,"refused":12,"cancelled":7,"errorRate":0.0121,"inputTokens":91234,"outputTokens":40112,"requestsWithTokens":1190,
  "latencyMs":{"p50":420,"p95":2800,"p99":9100,"approximate":true},"queueWaitMsAvg":12},
 "groups":[{"principal":{"kind":"api-key","id":"key_a","name":"my editor"},"requests":800,"failed":1,"refused":2,"cancelled":3,"errorRate":0.0038,"inputTokens":6,"outputTokens":3,"requestsWithTokens":800,
   "latencyMs":{"p50":400,"p95":2500,"p99":8000,"approximate":true},"queueWaitMsAvg":9},
  {"principal":{"kind":"browser","id":"browser:0123456789abcdef"},"requests":10,"failed":0,"refused":0,"cancelled":0,"errorRate":0,"inputTokens":0,"outputTokens":0,"requestsWithTokens":0,
   "latencyMs":{"p50":null,"p95":null,"p99":null,"approximate":true},"queueWaitMsAvg":0},
  {"model":"","requests":5,"failed":0,"refused":5,"cancelled":0,"errorRate":1,"inputTokens":0,"outputTokens":0,"requestsWithTokens":0,
   "latencyMs":{"p50":1,"p95":1,"p99":1,"approximate":true},"queueWaitMsAvg":0}],
 "series":[]}`

// fakeUsageAPI answers /_localbase/usage and records what the page asked.
type fakeUsageAPI struct {
	reads []string
	sent  []map[string]any
	reply string
	fail  string
}

func (f *fakeUsageAPI) call(_ context.Context, _ *auth.Principal, method, path, _ string, body []byte) APIResult {
	if !strings.HasPrefix(path, "/_localbase/usage") {
		return APIResult{Status: 200, Body: []byte(`{}`)}
	}
	if method == "GET" {
		f.reads = append(f.reads, path)
		return APIResult{Status: 200, Body: []byte(f.reply)}
	}
	var in map[string]any
	json.Unmarshal(body, &in)
	f.sent = append(f.sent, in)
	if f.fail != "" {
		return APIResult{Status: 403, Body: []byte(`{"error":{"message":"` + f.fail + `"}}`)}
	}
	return APIResult{Status: 200, Body: []byte(`{}`)}
}

func usageUI(t *testing.T, api *fakeUsageAPI, perms ...string) *UI {
	t.Helper()
	if api.reply == "" {
		api.reply = usageReply
	}
	p := person(append([]string{"models:read"}, perms...)...)
	base := connectUI(t, p, &fakeKeysAPI{}, "127.0.0.1:2273")
	d := base.d
	d.Call = api.call
	return New(d)
}

func TestUsagePage(t *testing.T) {
	api := &fakeUsageAPI{}
	u := usageUI(t, api)
	w, body := connectBody(t, u, "/app/usage")
	if w.Code != 200 {
		t.Fatalf("%d", w.Code)
	}
	for _, want := range []string{usageNoteLine, "1,240", "15 (1.2%)", "~420 ms", "~2.8 s", "~9.1 s", "91,234", "40,112", "1,190 requests that reported them",
		"kept for 30 days", "my editor", "Signed-in person 01234567", "No model named", `href="/app/connect"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if len(api.reads) != 1 || !strings.Contains(api.reads[0], "scope=self") || !strings.Contains(api.reads[0], "range=7d") {
		t.Errorf("reads: %v", api.reads)
	}
	// Anyone signed in gets here; only those allowed are offered more.
	if strings.Contains(body, "Everything") || strings.Contains(body, `hx-post="/app/usage/change"`) {
		t.Error("offers what this person cannot do")
	}
	// Whoever cannot clear is told what works (DESIGN §32.5).
	for _, want := range []string{"data-usage-cannot-clear", "system:manage", "access:manage", "local-base usage clear"} {
		if !strings.Contains(body, want) {
			t.Errorf("cannot-clear: missing %q", want)
		}
	}
}

// Every state has the line (DESIGN §32.8).
func TestUsagePageNote(t *testing.T) {
	empty := strings.NewReplacer(`"requests":1240`, `"requests":0`, `"requestsWithTokens":1190`, `"requestsWithTokens":0`).Replace(usageReply)
	off := strings.Replace(usageReply, `"recording":true`, `"recording":false`, 1)
	for name, reply := range map[string]string{"usual": usageReply, "empty": empty, "recording off": off, "cannot read": "not json"} {
		u := usageUI(t, &fakeUsageAPI{reply: reply})
		_, body := connectBody(t, u, "/app/usage")
		if !strings.Contains(body, usageNoteLine) {
			t.Errorf("%s: the line is missing", name)
		}
	}
	if usageNoteLine != "Counts only, stored on this computer, never prompts or replies; turn off or clear any time" {
		t.Error("the operator set this wording")
	}
	_, body := connectBody(t, usageUI(t, &fakeUsageAPI{reply: off}), "/app/usage")
	if !strings.Contains(body, "Recording is off") {
		t.Error("recording off should say so")
	}
	_, body = connectBody(t, usageUI(t, &fakeUsageAPI{reply: empty}), "/app/usage")
	if !strings.Contains(body, "No requests in this period") {
		t.Error("the empty state is missing")
	}
	_, body = connectBody(t, usageUI(t, &fakeUsageAPI{reply: "not json"}), "/app/usage")
	if !strings.Contains(body, "Usage unavailable") {
		t.Error("a failed read should say so")
	}
}

func TestUsagePageScopes(t *testing.T) {
	for _, perm := range []string{"models:manage", "system:read"} {
		api := &fakeUsageAPI{}
		u := usageUI(t, api, perm)
		_, body := connectBody(t, u, "/app/usage?scope=all&group=principal&range=30d")
		if !strings.Contains(body, "Everything") || !strings.Contains(body, ">Who<") {
			t.Errorf("%s: the choice is missing", perm)
		}
		if last := api.reads[len(api.reads)-1]; !strings.Contains(last, "scope=all") || !strings.Contains(last, "group=principal") || !strings.Contains(last, "range=30d") {
			t.Errorf("%s: read %s", perm, last)
		}
	}
	// Without the permission, asking for everything shows their own. Nothing from the URL passes unchecked.
	api := &fakeUsageAPI{}
	u := usageUI(t, api)
	connectBody(t, u, "/app/usage?scope=all&group=principal&range=99y")
	if last := api.reads[0]; !strings.Contains(last, "scope=self") || strings.Contains(last, "principal") || strings.Contains(last, "99y") {
		t.Errorf("read %s", last)
	}
}

func usagePost(u *UI, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/app/usage/change", strings.NewReader(form.Encode()))
	r.Host = "127.0.0.1:2273"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("HX-Request", "true")
	r.Header.Set("X-Localbase-Ui", "1")
	withCSRF(u, r)
	w := httptest.NewRecorder()
	u.ServeHTTP(w, r)
	return w
}

func TestUsagePageChanges(t *testing.T) {
	for _, perm := range []string{"system:manage", "access:manage"} {
		api := &fakeUsageAPI{}
		u := usageUI(t, api, perm)
		_, body := connectBody(t, u, "/app/usage")
		for _, want := range []string{`hx-post="/app/usage/change"`, "Clear all totals", "Turn recording off", `name="days"`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: missing %q", perm, want)
			}
		}
		if strings.Contains(body, "data-usage-cannot-clear") {
			t.Errorf("%s: told it cannot clear", perm)
		}
		for form, want := range map[string]string{
			"action=clear":                 `{"action":"clear"}`,
			"action=retention&days=7":      `{"action":"set-retention","days":7}`,
			"action=recording&enabled=off": `{"action":"set-recording","enabled":false}`,
			"action=recording&enabled=on":  `{"action":"set-recording","enabled":true}`,
		} {
			values, _ := url.ParseQuery(form)
			api.sent = nil
			w := usagePost(u, values)
			if w.Code != 200 || len(api.sent) != 1 {
				t.Fatalf("%s: %d, sent %v", form, w.Code, api.sent)
			}
			got, _ := json.Marshal(api.sent[0])
			var a, b any
			json.Unmarshal(got, &a)
			json.Unmarshal([]byte(want), &b)
			x, _ := json.Marshal(a)
			y, _ := json.Marshal(b)
			if string(x) != string(y) {
				t.Errorf("%s: sent %s, want %s", form, x, y)
			}
			if strings.Contains(w.Body.String(), "<html") || !strings.Contains(w.Body.String(), `id="usage-body"`) {
				t.Errorf("%s: the response is not the page's body", form)
			}
		}
	}
	api := &fakeUsageAPI{fail: "Forbidden: The credential lacks the required permission."}
	u := usageUI(t, api, "system:manage")
	if w := usagePost(u, url.Values{"action": {"clear"}}); !strings.Contains(w.Body.String(), "lacks the required permission") {
		t.Errorf("the API's refusal should show: %s", w.Body.String())
	}
	api = &fakeUsageAPI{}
	u = usageUI(t, api, "system:manage")
	if w := usagePost(u, url.Values{"action": {"retention"}, "days": {"soon"}}); len(api.sent) != 0 || !strings.Contains(w.Body.String(), "whole number") {
		t.Errorf("not a number: sent %v", api.sent)
	}
	if w := usagePost(u, url.Values{"action": {"nope"}}); w.Code != http.StatusBadRequest {
		t.Errorf("an unknown action: %d", w.Code)
	}
	// Same-origin and CSRF, as every change.
	r := httptest.NewRequest("POST", "/app/usage/change", strings.NewReader("action=clear"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("HX-Request", "true")
	r.Header.Set("X-Localbase-Ui", "1")
	w := httptest.NewRecorder()
	api.sent = nil
	u.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || len(api.sent) != 0 {
		t.Errorf("no CSRF token: %d, sent %v", w.Code, api.sent)
	}
}

// Both ways to it, for people with and without Admin (DESIGN §32.8).
func TestUsageLinks(t *testing.T) {
	_, settings := connectBody(t, usageUI(t, &fakeUsageAPI{}), "/app/settings")
	if !strings.Contains(settings, `href="/app/usage"`) || !strings.Contains(settings, "Your usage") {
		t.Error("Settings needs a Your usage row")
	}
	_, admin := connectBody(t, usageUI(t, &fakeUsageAPI{}, "keys:read", "keys:manage"), "/app/admin")
	if !strings.Contains(admin, `href="/app/usage"`) {
		t.Error("Admin needs a link")
	}
}
