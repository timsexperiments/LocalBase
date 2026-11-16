package web

// Tests for the web review's must-fix findings (03-M2, M3, M4, M7), each failing on main 10838062.

import (
	"bytes"
	"context"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
)

// 03-M2: first run's GETs finish only an install this person started here. A GET for an installed model
// nobody started (a cross-site link, say) changes nothing and goes to the Library.
func TestWelcomeGETsFinishOnlyThePersonsOwnInstall(t *testing.T) {
	cat, _ := catalog.Load("")
	var chat string
	for _, s := range cat.All() {
		if tk, _ := task(s); tk == "Chat" {
			chat = s.ModelID
			break
		}
	}
	var actions []string
	u := New(Deps{NoAuth: true, Catalog: cat, RootDir: installedRoot(t, cat, chat),
		Call: func(_ context.Context, _ *auth.Principal, method, path, _ string, body []byte) APIResult {
			if method == "POST" {
				var in struct{ Action string }
				json.Unmarshal(body, &in)
				actions = append(actions, in.Action)
			}
			return APIResult{Status: 200, Body: []byte(`{"state":"complete","models":[{"id":"` + chat + `","installed":true}]}`)}
		}})
	for _, path := range []string{"/app/welcome/getting?model=" + chat, "/app/welcome/progress?model=" + chat} {
		actions = nil
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("HX-Request", "true")
		req.Header.Set("X-Localbase-UI", "1")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		w := httptest.NewRecorder()
		u.ServeHTTP(w, req)
		if len(actions) != 0 {
			t.Errorf("%s: a GET nobody started changed the model: %v", path, actions)
		}
		if to := w.Header().Get("HX-Redirect") + w.Header().Get("Location"); to != "/app/models" {
			t.Errorf("%s: sent to %q (%d), want the Library", path, to, w.Code)
		}
	}
	// The person's own install, started by the POST, is still finished by the progress poll.
	started := httptest.NewRequest("POST", "/app/welcome/get", nil)
	u.chose(started.WithContext(context.WithValue(started.Context(), ctxKey{}, u.principal(started))), chat)
	actions = nil
	req := httptest.NewRequest("GET", "/app/welcome/progress?model="+chat, nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	u.ServeHTTP(w, req)
	if len(actions) != 1 || actions[0] != "activate" || w.Header().Get("HX-Redirect") != "/app/chat?ready=1" {
		t.Errorf("the person's own install: actions %v, redirect %q", actions, w.Header().Get("HX-Redirect"))
	}
}

// 03-M3: a template that fails partway sends a 500 with none of its partial output, and the error is logged.
func TestTemplateFailureIsWholeAndLogged(t *testing.T) {
	var logs bytes.Buffer
	u := New(Deps{NoAuth: true, Log: slog.New(slog.NewTextHandler(&logs, nil))})
	u.pages["broken"] = template.Must(template.New("broken").Parse(`{{define "layout"}}<p>part of a page</p>{{index .Items 5}}{{end}}{{define "piece"}}<p>part</p>{{index .Items 5}}{{end}}`))
	for _, do := range []func(http.ResponseWriter, *http.Request){
		func(w http.ResponseWriter, r *http.Request) {
			u.render(w, r, "broken", map[string]any{"Items": []int{}})
		},
		func(w http.ResponseWriter, r *http.Request) {
			u.fragment(w, r, "broken", "piece", map[string]any{"Items": []int{}})
		},
		func(w http.ResponseWriter, r *http.Request) { u.fragment(w, r, "no-such-page", "piece", nil) },
	} {
		logs.Reset()
		w := httptest.NewRecorder()
		do(w, httptest.NewRequest("GET", "/app/x", nil))
		if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "part") {
			t.Errorf("a failed template sent %d: %q", w.Code, w.Body.String())
		}
		if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "template") {
			t.Errorf("the failure was not logged: %q", logs.String())
		}
	}
}

// 03-M4: steps are sent as the number they mean, so the body is valid JSON; zero and negatives are refused.
func TestImageStepsAreANumber(t *testing.T) {
	form := func(steps string) *http.Request {
		r := httptest.NewRequest("POST", "/", strings.NewReader(url.Values{"prompt": {"a boat"}, "steps": {steps}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return r
	}
	for steps, want := range map[string]float64{"+5": 5, "007": 7, "5": 5} {
		q, msg := imageForm(form(steps))
		var sent map[string]any
		if msg != "" || json.Unmarshal(q.body(), &sent) != nil || sent["steps"] != want {
			t.Errorf("steps %q: msg %q, body %s", steps, msg, q.body())
		}
	}
	for _, steps := range []string{"0", "-3", "abc"} {
		if _, msg := imageForm(form(steps)); msg != "steps must be a whole number." {
			t.Errorf("steps %q: %q", steps, msg)
		}
	}
}

// 03-M7: saving a role that doesn't exist says so, and sends nothing.
func TestSavingAMissingRoleSaysSo(t *testing.T) {
	u, f, _ := adminUI("models:read", "access:read", "access:manage")
	before := len(f.sent)
	out := adminPost(u, "/app/admin/access", url.Values{"section": {"people"}, "action": {"save-role"}, "name": {"ghost"}, "description": {"x"}}).Body.String()
	if !strings.Contains(out, "No role named ghost.") || strings.Contains(out, "Role ghost saved.") {
		t.Errorf("saving a missing role: %s", out[:min(len(out), 300)])
	}
	if len(f.sent) != before {
		t.Errorf("it still sent %v", f.sent[before:])
	}
	adminPost(u, "/app/admin/access", url.Values{"section": {"people"}, "action": {"save-role"}, "name": {"chat"}, "description": {"Talks"}})
	if len(f.sent) != before+1 || f.sent[before]["action"] != "apply-policy" {
		t.Errorf("saving a real role sent %v", f.sent[before:])
	}
}
