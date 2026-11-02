package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/connect"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

const (
	connectModelA = "qwen3.5-4b-q4_k_m"
	connectModelB = "qwen3.8-27b-q4_k_m"
)

// fakeKeysAPI answers /_localbase/api-keys as the gateway does.
type fakeKeysAPI struct {
	sent   []map[string]any
	fail   string
	secret string
}

func (f *fakeKeysAPI) call(_ context.Context, p *auth.Principal, method, path, _ string, body []byte) APIResult {
	if path == "/_localbase/api-keys" && method == "POST" {
		if !p.Can("keys:manage") {
			return APIResult{Status: 403, Body: []byte(`{"error":{"message":"forbidden"}}`)}
		}
		var in map[string]any
		json.Unmarshal(body, &in)
		f.sent = append(f.sent, in)
		if f.fail != "" {
			return APIResult{Status: 400, Body: []byte(`{"error":{"message":"` + f.fail + `"}}`)}
		}
		return APIResult{Status: 200, Body: []byte(`{"key":{"id":"key_1"},"secret":"` + f.secret + `"}`)}
	}
	return APIResult{Status: 200, Body: []byte(`{}`)}
}

// connectUI renders the page on a scratch root with the given chat models installed.
func connectUI(t *testing.T, p *auth.Principal, api *fakeKeysAPI, listen string, installed ...string) *UI {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	models := filepath.Join(dir, "models", "llm")
	os.MkdirAll(models, 0o700)
	cfg := &store.Config{Root: dir, CtxSize: 8192, CtxMode: "auto", Parallel: "1", Active: map[string]string{}, Selected: map[string][]string{"llm": {}}}
	for i, id := range installed {
		s := cat.ByID(id)
		if s == nil {
			t.Fatalf("no catalog entry %s", id)
		}
		for _, a := range s.Artifacts {
			f, err := os.Create(filepath.Join(models, a.Filename))
			if err != nil {
				t.Fatal(err)
			}
			f.Truncate(a.ExpectedSizeBytes)
			f.Close()
		}
		cfg.Selected["llm"] = append(cfg.Selected["llm"], id)
		if i == 0 {
			cfg.Active["llm"] = id
		}
	}
	mem := memory.NewController(memory.NewHostProvider(), memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	llm := runtime.NewLLM(runtime.LLMDeps{Catalog: cat, Store: st, Memory: mem, ModelsDir: models, MemoryGB: 64, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, cfg)
	t.Cleanup(llm.Close)
	d := Deps{NoAuth: p == nil, Call: api.call, Catalog: cat, Store: st, LLM: llm, RootDir: dir, Memory: mem,
		ListenAddr: func() string { return listen }}
	if p != nil {
		d.SignInEnabled = true
		d.Principal = func(*http.Request) *auth.Principal { return p }
	}
	return New(d)
}

func person(perms ...string) *auth.Principal {
	return &auth.Principal{Kind: "browser-session", ID: "u", Name: "someone@example.test", Permissions: perms}
}

func connectBody(t *testing.T, u *UI, target string, headers ...string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	r := httptest.NewRequest("GET", target, nil)
	r.Host = "127.0.0.1:2273"
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i] == "Host" {
			r.Host = headers[i+1]
			continue
		}
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	u.ServeHTTP(w, r)
	return w, w.Body.String()
}

func connectPost(u *UI, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/app/connect/key", strings.NewReader(form.Encode()))
	r.Host = "127.0.0.1:2273"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("HX-Request", "true")
	r.Header.Set("X-Localbase-Ui", "1")
	withCSRF(u, r)
	w := httptest.NewRecorder()
	u.ServeHTTP(w, r)
	return w
}

func TestConnectPage(t *testing.T) {
	u := connectUI(t, person("models:read"), &fakeKeysAPI{}, "127.0.0.1:2273", connectModelA)
	w, body := connectBody(t, u, "/app/connect")
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	for _, want := range []string{"Connect an app", "http://127.0.0.1:2273/v1", connectModelA, connect.KeyToken,
		"opencode", "Continue", "Cline", "Aider", "Zed", "goose", "Open WebUI", "Python (OpenAI SDK)", "JavaScript (OpenAI SDK)",
		"Codex CLI", "/openapi.json", `href="/app/connect"`, "js/connect.js"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	// What nobody has run says so, on every snippet of every such client, and is not
	// claimed for what has been run.
	notRun := 0
	for _, c := range connect.Clients(connect.Target{BaseURL: "http://127.0.0.1:2273/v1"}) {
		if !c.Run {
			notRun += len(c.Snippets)
		}
	}
	if got := strings.Count(body, "data-not-run>Follows the client&#39;s docs, not run."); got != notRun || notRun == 0 {
		t.Errorf("%d not-run labels, want %d", got, notRun)
	}
	curl := body[strings.Index(body, `data-client="curl"`):strings.Index(body, `data-client="python"`)]
	if strings.Contains(curl, "not run") || strings.Contains(curl, "Not run") {
		t.Error("curl has been run, and is marked as not")
	}
	zed := body[strings.Index(body, `data-client="zed"`):strings.Index(body, `data-client="goose"`)]
	if !strings.Contains(zed, ">Not run<") {
		t.Error("a client nobody has run needs its label while closed")
	}
	// Someone who cannot create a key is told who can, and gets the command.
	if strings.Contains(body, `hx-post="/app/connect/key"`) {
		t.Error("offers to create a key without keys:manage")
	}
	if !strings.Contains(body, "local-base keys create --name my-app --scopes inference:chat,models:read") {
		t.Error("the command for someone who cannot create a key is missing")
	}
	// The first client is open; the rest are closed until asked for.
	if !regexp.MustCompile(`data-client="curl" open`).MatchString(body) || regexp.MustCompile(`data-client="zed" open`).MatchString(body) {
		t.Error("the wrong client is open")
	}
	if _, zed := connectBody(t, u, "/app/connect?client=zed"); !strings.Contains(zed, `data-client="zed" open`) {
		t.Error("?client= does not open one")
	}
	// A client nobody knows is ignored, not echoed.
	if _, bad := connectBody(t, u, "/app/connect?client=%3Cb%3Eevil"); strings.Contains(bad, "evil") {
		t.Error("an unknown client is echoed")
	}
}

func TestConnectNeedsSignIn(t *testing.T) {
	u := connectUI(t, &auth.Principal{Kind: "anonymous"}, &fakeKeysAPI{}, "127.0.0.1:2273")
	if w, _ := connectBody(t, u, "/app/connect"); w.Code != http.StatusSeeOther {
		t.Errorf("status %d", w.Code)
	}
	reader := connectUI(t, person("inference:chat"), &fakeKeysAPI{}, "127.0.0.1:2273")
	if w, _ := connectBody(t, reader, "/app/connect"); w.Code != http.StatusForbidden {
		t.Errorf("without models:read: %d", w.Code)
	}
}

func TestConnectNoModelYet(t *testing.T) {
	u := connectUI(t, person("models:read"), &fakeKeysAPI{}, "127.0.0.1:2273")
	_, body := connectBody(t, u, "/app/connect")
	if !strings.Contains(body, connect.ModelToken) || !strings.Contains(body, `href="/app/models/get"`) {
		t.Error("with no model, the page should say so and link to getting one")
	}
	// Copy never names a model (DESIGN §28.3): with none installed, none can appear.
	cat, _ := catalog.Load("")
	main := body[strings.Index(body, `id="connect-title"`):]
	for _, s := range cat.All() {
		if strings.Contains(main, s.ModelID) {
			t.Errorf("the page names %s", s.ModelID)
		}
	}
}

func TestConnectChoices(t *testing.T) {
	u := connectUI(t, person("models:read"), &fakeKeysAPI{}, "127.0.0.1:2273", connectModelA, connectModelB)
	_, body := connectBody(t, u, "/app/connect")
	if !strings.Contains(body, `name="model"`) {
		t.Fatal("two models offer a choice")
	}
	_, other := connectBody(t, u, "/app/connect?model="+connectModelB)
	if !strings.Contains(other, `\"`+connectModelB) && !strings.Contains(other, "&#34;"+connectModelB) {
		t.Errorf("the model chosen is not in the snippets")
	}
	// Changing it swaps only the snippets, so the choosers keep their focus.
	w, frag := connectBody(t, u, "/app/connect?model="+connectModelB, "HX-Request", "true", "HX-Target", "connect-snippets")
	if w.Code != 200 || strings.Contains(frag, "<html") || !strings.Contains(frag, `id="connect-snippets"`) || !strings.Contains(frag, connectModelB) {
		t.Errorf("fragment %d: %.200s", w.Code, frag)
	}
	// Nothing from the URL reaches a snippet unless it is on the list.
	_, bad := connectBody(t, u, "/app/connect?model=evil-model&base=https%3A%2F%2Fevil.example%2Fv1")
	for _, s := range []string{"evil-model", "evil.example"} {
		if strings.Contains(bad, s) {
			t.Errorf("%s reached the page", s)
		}
	}
}

func TestConnectAddresses(t *testing.T) {
	access := &auth.BrowserAccess{Provider: auth.Provider{Kind: "cloudflare-access", TeamDomain: "team.cloudflareaccess.com", Audience: "aud"}, Origin: "https://gateway.example"}
	u := connectUI(t, person("models:read"), &fakeKeysAPI{}, "0.0.0.0:2273")
	if err := auth.SaveAccess(u.d.RootDir, access); err != nil {
		t.Fatal(err)
	}
	_, body := connectBody(t, u, "/app/connect")
	for _, want := range []string{"http://127.0.0.1:2273/v1", "https://gateway.example/v1", "Cloudflare Access", "other addresses"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	// Opened at the public address, that is where the snippets start.
	_, public := connectBody(t, u, "/app/connect", "Host", "gateway.example", "X-Forwarded-Proto", "https")
	if !strings.Contains(public, `value="https://gateway.example/v1" selected`) {
		t.Error("the snippets should start with the address this page is open at")
	}
	// Opened at some other address, it is added and comes first in the snippets.
	_, lan := connectBody(t, u, "/app/connect", "Host", "192.168.1.5:2273")
	if !strings.Contains(lan, "This page&#39;s address") || !strings.Contains(lan, `value="http://192.168.1.5:2273/v1" selected`) {
		t.Error("this page's address is missing")
	}
	// Never for a loopback name: this computer already covers it.
	if _, local := connectBody(t, u, "/app/connect", "Host", "localhost:2273"); strings.Contains(local, "This page&#39;s address") {
		t.Error("a loopback name is added as a separate address")
	}
}

func TestConnectCreateKey(t *testing.T) {
	const secret = "lb_connect_test_secret_0123456789"
	api := &fakeKeysAPI{secret: secret}
	u := connectUI(t, person("models:read", "keys:manage"), api, "127.0.0.1:2273", connectModelA)
	_, page := connectBody(t, u, "/app/connect")
	if !strings.Contains(page, `hx-post="/app/connect/key"`) || !strings.Contains(page, "Create a key for chat") {
		t.Fatal("the one tap is missing")
	}
	w := connectPost(u, url.Values{"preset": {"chat"}})
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if len(api.sent) != 1 {
		t.Fatalf("sent %v", api.sent)
	}
	got := api.sent[0]
	scopes, _ := got["scopes"].([]any)
	if got["action"] != "create" || len(scopes) != 2 || scopes[0] != "inference:chat" || scopes[1] != "models:read" {
		t.Errorf("one tap sent %v, want exactly chat and models:read", got)
	}
	if name, _ := got["name"].(string); name == "" {
		t.Error("the key needs a name")
	}
	body := w.Body.String()
	if !strings.Contains(body, `data-connect-secret>`+secret) {
		t.Error("the secret is not shown")
	}
	if strings.Contains(body, connect.KeyToken) {
		t.Error("the snippets still hold the token beside a real key")
	}
	if strings.Contains(body, "<html") || !strings.Contains(body, `id="connect-body"`) {
		t.Errorf("the response is not the page's body: %.100s", body)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if strings.Contains(w.Header().Get("HX-Push-Url")+w.Header().Get("Location"), secret) {
		t.Error("the secret reached a URL")
	}
	// Once only.
	if _, later := connectBody(t, u, "/app/connect"); strings.Contains(later, secret) {
		t.Error("a later GET shows the secret")
	}
}

func TestConnectPresets(t *testing.T) {
	api := &fakeKeysAPI{secret: "lb_x"}
	u := connectUI(t, person("models:read", "keys:manage"), api, "127.0.0.1:2273")
	// A key never gets administration from here.
	for _, p := range connectPresets {
		for _, s := range p.Scopes {
			if !slices.Contains(auth.DefaultKeyScopes, s) {
				t.Errorf("preset %s grants %s", p.ID, s)
			}
		}
	}
	for _, p := range connectPresets {
		api.sent = nil
		if w := connectPost(u, url.Values{"preset": {p.ID}, "name": {"My editor"}}); w.Code != 200 {
			t.Errorf("%s: %d", p.ID, w.Code)
			continue
		}
		var scopes []string
		list, _ := api.sent[0]["scopes"].([]any)
		for _, s := range list {
			scopes = append(scopes, s.(string))
		}
		if !slices.Equal(scopes, p.Scopes) || api.sent[0]["name"] != "My editor" {
			t.Errorf("%s: sent %v", p.ID, api.sent[0])
		}
	}
	api.sent = nil
	if w := connectPost(u, url.Values{"preset": {"everything"}}); w.Code != http.StatusBadRequest || len(api.sent) != 0 {
		t.Errorf("an unknown preset: %d, sent %v", w.Code, api.sent)
	}
}

func TestConnectKeyRefusals(t *testing.T) {
	const secret = "lb_never_shown"
	// Without keys:manage.
	api := &fakeKeysAPI{secret: secret}
	reader := connectUI(t, person("models:read"), api, "127.0.0.1:2273")
	if w := connectPost(reader, url.Values{"preset": {"chat"}}); w.Code != http.StatusForbidden || len(api.sent) != 0 {
		t.Errorf("reader: %d, sent %v", w.Code, api.sent)
	}
	// Without the CSRF token.
	manager := connectUI(t, person("models:read", "keys:manage"), api, "127.0.0.1:2273")
	r := httptest.NewRequest("POST", "/app/connect/key", strings.NewReader("preset=chat"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("HX-Request", "true")
	r.Header.Set("X-Localbase-Ui", "1")
	w := httptest.NewRecorder()
	manager.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || len(api.sent) != 0 {
		t.Errorf("no CSRF token: %d, sent %v", w.Code, api.sent)
	}
	// The API's refusal is shown, with no secret.
	api.fail = "The name is too long."
	w = connectPost(manager, url.Values{"preset": {"chat"}})
	if body := w.Body.String(); w.Code != 200 || !strings.Contains(body, "The name is too long.") || strings.Contains(body, secret) {
		t.Errorf("refusal: %d %.200s", w.Code, body)
	}
}

func TestConnectNoAuth(t *testing.T) {
	u := connectUI(t, nil, &fakeKeysAPI{}, "127.0.0.1:2273")
	_, body := connectBody(t, u, "/app/connect")
	if !strings.Contains(body, "without sign-in") || !strings.Contains(body, noAuthKey) {
		t.Error("--no-auth should say so, and fill a placeholder")
	}
	if strings.Contains(body, `hx-post="/app/connect/key"`) || strings.Contains(body, connect.KeyToken) {
		t.Error("--no-auth has no key to create")
	}
}
