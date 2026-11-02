package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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

func TestInstallPace(t *testing.T) {
	var p installPace
	start := time.Unix(1000, 0)
	const unknown = "How long this takes depends on your connection."
	if got := p.left("m", start, 0, 3_000_000_000); got != unknown {
		t.Errorf("first look: %q", got)
	}
	if got := p.left("m", start.Add(10*time.Second), 200_000_000, 3_000_000_000); got != unknown {
		t.Errorf("too soon: %q", got)
	}
	// 20 MB/s, with 2.6 GB to go: 130 s.
	if got := p.left("m", start.Add(20*time.Second), 400_000_000, 3_000_000_000); got != "About 3 minutes left." {
		t.Errorf("measured: %q", got)
	}
	if got := p.left("slow", start, 0, 3_000_000_000); got != unknown {
		t.Errorf("first look: %q", got)
	}
	if got := p.left("slow", start.Add(30*time.Second), 1_000_000, 3_000_000_000); got != unknown {
		t.Errorf("too little moved to say: %q", got)
	}
}

func TestInstallFailureSaysWhy(t *testing.T) {
	const generic = "Installation failed. Check storage, network access, and catalog verification."
	if got := installFailure(generic, 3_000_000_000, 1_000_000_000); !strings.Contains(got, "enough disk space") {
		t.Errorf("disk: %q", got)
	}
	if got := installFailure(generic, 3_000_000_000, 9_000_000_000); !strings.Contains(got, "usually the connection") {
		t.Errorf("connection: %q", got)
	}
	if got := installFailure("Interrupted when the gateway stopped. Install again to resume.", 3_000_000_000, 9_000_000_000); !strings.Contains(got, "pick up") {
		t.Errorf("interrupted: %q", got)
	}
}

// The first download is the smallest that fits, and "Small download" is only about size.
func TestChooseFromPicksTheSmallest(t *testing.T) {
	rows := []CatalogRow{
		{ID: "big", DownloadBytes: 20_000_000_000, Fit: "fits"},
		{ID: "huge", DownloadBytes: 1_000_000_000, Fit: "wont-fit"},
		{ID: "small", DownloadBytes: 3_000_000_000, Fit: "tight"},
		{ID: "mid", DownloadBytes: 8_000_000_000, Fit: "fits"},
	}
	picked := func(c []welcomeChoice) (id string) {
		for _, x := range c {
			if x.Pick {
				id += x.ID
			}
			if x.Small != (x.DownloadBytes < smallDownload) {
				t.Errorf("%s: small %v", x.ID, x.Small)
			}
		}
		return id
	}
	if got := picked(chooseFrom(rows, 100_000_000_000)); got != "small" {
		t.Errorf("plenty of disk: %q", got)
	}
	if got := picked(chooseFrom(rows, 1_000_000_000)); got != "" {
		t.Errorf("nothing fits the disk: %q", got)
	}
}

// Signed in with no permissions: every page says so, offers Sign out, and offers nothing else.
func TestNoAccessPage(t *testing.T) {
	cat, _ := catalog.Load("")
	p := &auth.Principal{Kind: "browser-session", ID: "u", Name: "someone@example.com", Permissions: []string{}}
	u := New(Deps{SignInEnabled: true, Principal: func(*http.Request) *auth.Principal { return p }, Catalog: cat, RootDir: t.TempDir()})
	for _, path := range []string{"/app/chat", "/app/models", "/app/studio", "/app/settings", "/app/admin", "/app/welcome", "/app/nope"} {
		w := httptest.NewRecorder()
		u.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		body := w.Body.String()
		if w.Code != 403 || !strings.Contains(body, "doesn’t have access yet") || !strings.Contains(body, "someone@example.com") || !strings.Contains(body, "data-logout") {
			t.Errorf("%s: %d %s", path, w.Code, body)
		}
		if !strings.Contains(body, `href="/app/login?switch=1"`) {
			t.Errorf("%s: no way to use another account", path)
		}
		if strings.Contains(body, "tabbar") || strings.Contains(body, "/app/models/running") {
			t.Errorf("%s offers somewhere to go", path)
		}
	}
}

// Signing out stays out: it doesn't go back to a provider that would sign you straight in.
func TestSignedOutPage(t *testing.T) {
	u := New(Deps{SignInEnabled: true, Principal: func(*http.Request) *auth.Principal { return nil }})
	w := httptest.NewRecorder()
	u.ServeHTTP(w, httptest.NewRequest("GET", "/app?signedout=1", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "You’re signed out") || !strings.Contains(w.Body.String(), `href="/app/login"`) || !strings.Contains(w.Body.String(), `href="/app/login?switch=1"`) {
		t.Errorf("%d %s", w.Code, w.Body.String())
	}
	// Local sign-in has no other account to choose.
	local := New(Deps{Principal: func(*http.Request) *auth.Principal { return nil }})
	w = httptest.NewRecorder()
	local.ServeHTTP(w, httptest.NewRequest("GET", "/app?signedout=1", nil))
	if strings.Contains(w.Body.String(), "switch=1") {
		t.Errorf("local: %s", w.Body.String())
	}
}

// A sign-in that failed says so, rather than starting another one.
func TestSignInFailedPage(t *testing.T) {
	u := New(Deps{SignInEnabled: true, Principal: func(*http.Request) *auth.Principal { return nil }})
	w := httptest.NewRecorder()
	u.ServeHTTP(w, httptest.NewRequest("GET", "/app?signin=failed", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "didn’t work") || !strings.Contains(w.Body.String(), `href="/app/login"`) {
		t.Errorf("%d %s", w.Code, w.Body.String())
	}
}

// The strip shows the gateway's reading. An unknown figure is no picture, never a 0.
func TestStripReadsTheHost(t *testing.T) {
	t.Setenv("LOCALBASE_COMPUTER_NAME", "")
	cat, _ := catalog.Load("")
	reply := `{"host":{"name":"Attic Mac","nameSource":"setting","memory":{"total":{"bytes":68719476736,"source":"system"},"available":{"bytes":32212254720,"source":"system"},"pressure":"healthy","other":{"bytes":21474836480,"source":"derived"},"localBase":{"bytes":15032385536,"source":"mixed","runtimes":[]}},"activity":{"busy":true,"runs":{"active":1}}}}`
	api := func(_ context.Context, _ *auth.Principal, _, _, _ string, _ []byte) APIResult {
		return APIResult{Status: 200, Body: []byte(reply)}
	}
	p := &auth.Principal{Kind: "browser-session", ID: "u", Permissions: []string{"models:read"}}
	u := New(Deps{SignInEnabled: true, Principal: func(*http.Request) *auth.Principal { return p }, Catalog: cat, RootDir: t.TempDir(), Call: api})
	r := httptest.NewRequest("GET", "/app/strip", nil).WithContext(context.WithValue(context.Background(), ctxKey{}, p))
	strip := u.memoryStrip(r)
	if strip["Host"] != "Attic Mac" || strip["Word"] != "Busy" || strip["Free"] != "30 GiB free" || strip["Source"] != "measured and planned" || strip["Known"] != true {
		t.Errorf("%+v", strip)
	}
	if got := gaugeSegments(strip["Other"].(int64), strip["Models"].(int64), strip["Capacity"].(int64)); got[0].Kind != "other" || got[20].Kind != "models" {
		t.Errorf("segments %+v, %+v", got[0], got[20])
	}
	reply = `{"host":{"name":null,"memory":{"total":{"bytes":null,"source":"unavailable"},"available":{"bytes":null,"source":"unavailable"},"pressure":null,"other":{"bytes":null,"source":"unavailable"},"localBase":{"bytes":0,"source":"none","runtimes":[]}},"activity":{"busy":false}}}`
	strip = u.memoryStrip(r)
	if strip["Known"] != false || strip["Free"] != "Memory unavailable" || strip["Word"] != "Ready" {
		t.Errorf("no reading: %+v", strip)
	}
	if got := gaugeSegments(strip["Other"].(int64), strip["Models"].(int64), strip["Capacity"].(int64)); got[0].Kind != "unknown" {
		t.Errorf("no reading drew %+v", got[0])
	}
}

// Fit is the gateway's answer for now, in words that say so; a row keeps its old answer without one.
func TestForecast(t *testing.T) {
	answers := map[string]string{
		"a": `{"admitted":true,"requestedBytes":4000000000}`,
		"b": `{"admitted":false,"refusal":{"code":"insufficient_memory","permanent":false}}`,
		"c": `{"admitted":false,"refusal":{"code":"model_too_large","permanent":true}}`,
	}
	u := New(Deps{Call: func(_ context.Context, _ *auth.Principal, _, path, _ string, _ []byte) APIResult {
		for id, a := range answers {
			if strings.Contains(path, "/models/"+id+"/admission?assumeInstalled=1") {
				return APIResult{Status: 200, Body: []byte(a)}
			}
		}
		return APIResult{Status: 500}
	}})
	rows := []CatalogRow{{ID: "a", Fit: "tight"}, {ID: "b", Fit: "fits"}, {ID: "c", Fit: "fits"}, {ID: "d", Fit: "tight"}, {ID: "a", Fit: "fits", Installed: true}}
	got := u.withForecast(httptest.NewRequest("GET", "/", nil), rows)
	for i, want := range []struct {
		fit      string
		forecast bool
	}{{"fits", true}, {"close-others", true}, {"wont-fit", true}, {"tight", false}, {"fits", false}} {
		if got[i].Fit != want.fit || got[i].Forecast != want.forecast {
			t.Errorf("%d: %+v", i, got[i])
		}
	}
	if got[0].UsesBytes != 4000000000 {
		t.Errorf("uses %d", got[0].UsesBytes)
	}
}

// The Computer name setting saves through the API, and an empty name goes back to the machine's own.
func TestComputerNameSave(t *testing.T) {
	var sent string
	u := New(Deps{NoAuth: true, Call: func(_ context.Context, _ *auth.Principal, method, path, _ string, body []byte) APIResult {
		if method == "PUT" && path == "/_localbase/computer-name" {
			sent = string(body)
			return APIResult{Status: 200, Body: []byte(`{"name":"Attic Mac","nameSource":"setting"}`)}
		}
		return APIResult{Status: 200, Body: []byte(`{"host":{"name":"Attic Mac","nameSource":"setting"}}`)}
	}})
	w := httptest.NewRecorder()
	u.ServeHTTP(w, htmxPost("/app/settings/computer-name", url.Values{"name": {" Attic Mac "}, "action": {"save"}}))
	if sent != `{"name":"Attic Mac"}` || !strings.Contains(w.Body.String(), `value="Attic Mac"`) || !strings.Contains(w.Body.String(), "Saved.") {
		t.Errorf("save: %s: %s", sent, w.Body.String())
	}
	w = httptest.NewRecorder()
	u.ServeHTTP(w, htmxPost("/app/settings/computer-name", url.Values{"name": {"Attic Mac"}, "action": {"reset"}}))
	if sent != `{"name":""}` {
		t.Errorf("reset: %s", sent)
	}
}
