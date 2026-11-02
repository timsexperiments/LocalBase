package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
)

// installedRoot is a root where these models are on disk, as sparse files of their real size.
func installedRoot(t *testing.T, cat *catalog.Catalog, ids ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, id := range ids {
		s := cat.ByID(id)
		if s == nil {
			t.Fatalf("no model %s", id)
		}
		dir := filepath.Join(root, "models", s.Kind)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, a := range s.Artifacts {
			f, err := os.Create(filepath.Join(dir, a.Filename))
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Truncate(a.ExpectedSizeBytes); err != nil {
				t.Fatal(err)
			}
			f.Close()
		}
	}
	return root
}

// /app leads to the first-run hello only when there's no chat model and the person can add one.
func TestHomeLeadsToFirstRun(t *testing.T) {
	cat, _ := catalog.Load("")
	empty := New(Deps{NoAuth: true, Catalog: cat, RootDir: t.TempDir()})
	w := httptest.NewRecorder()
	empty.ServeHTTP(w, httptest.NewRequest("GET", "/app", nil))
	if got := w.Header().Get("Location"); got != "/app/welcome" {
		t.Errorf("empty root: %q", got)
	}
	var chat string
	for _, s := range cat.All() {
		if tk, _ := task(s); tk == "Chat" {
			chat = s.ModelID
			break
		}
	}
	ready := New(Deps{NoAuth: true, Catalog: cat, RootDir: installedRoot(t, cat, chat)})
	w = httptest.NewRecorder()
	ready.ServeHTTP(w, httptest.NewRequest("GET", "/app", nil))
	if got := w.Header().Get("Location"); got != "/app/chat" {
		t.Errorf("with %s: %q", chat, got)
	}
}

func TestInstallWords(t *testing.T) {
	for detail, want := range map[string]string{
		"download-progress":      "Downloading.",
		"verification-completed": "Checking each file as it arrives.",
		"Preparing installation": "Getting started.",
		"something new":          "Working on it.",
	} {
		if got := installWords(detail); got != want {
			t.Errorf("%s: %q, want %q", detail, got, want)
		}
	}
}

// An address that isn't one is a page in the app's own clothes, with a 404.
func TestNotFoundPage(t *testing.T) {
	u := New(Deps{NoAuth: true})
	w := httptest.NewRecorder()
	u.ServeHTTP(w, httptest.NewRequest("GET", "/app/nope", nil))
	if w.Code != 404 || !strings.Contains(w.Body.String(), "This page isn’t here") || !strings.Contains(w.Body.String(), `href="/app"`) {
		t.Fatalf("%d: %s", w.Code, w.Body.String())
	}
}

// A model already on disk is made the default with one call, and an older gateway that wants it
// enabled first still works. Chat gets a real page load: a swapped-in body would not start its scripts.
func TestWelcomeGetTurnsAnInstalledModelOn(t *testing.T) {
	cat, _ := catalog.Load("")
	var chat string
	for _, s := range cat.All() {
		if tk, _ := task(s); tk == "Chat" {
			chat = s.ModelID
			break
		}
	}
	for _, c := range []struct {
		name  string
		older bool
		want  []string
	}{
		{"today", false, []string{"activate"}},
		{"older gateway", true, []string{"activate", "enable", "activate"}},
	} {
		var actions []string
		u := New(Deps{NoAuth: true, Catalog: cat, RootDir: installedRoot(t, cat, chat),
			Call: func(_ context.Context, _ *auth.Principal, _, _, _ string, body []byte) APIResult {
				var in struct{ Action string }
				json.Unmarshal(body, &in)
				actions = append(actions, in.Action)
				if c.older && in.Action == "activate" && len(actions) == 1 {
					return APIResult{Status: 409, Body: []byte(`{"error":{"code":"conflict","message":"Enable the model before activating it."}}`)}
				}
				return APIResult{Status: 200, Body: []byte(`{"state":"complete"}`)}
			}})
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/app/welcome/get", strings.NewReader(url.Values{"modelId": {chat}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		req.Header.Set("X-Localbase-UI", "1")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		withCSRF(u, req)
		u.ServeHTTP(w, req)
		if !slices.Equal(actions, c.want) {
			t.Errorf("%s: actions %v, want %v", c.name, actions, c.want)
		}
		if got := w.Header().Get("HX-Redirect"); got != "/app/chat?ready=1" {
			t.Errorf("%s: redirect %q (%d)", c.name, got, w.Code)
		}
	}
}
