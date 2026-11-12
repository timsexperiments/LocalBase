package web

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/backends"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/library"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/testutil"
)

type backendPages struct {
	root *root.Root
	st   *store.Store
	lib  *library.Library
	logs *lockedLog
	ui   func(perms ...string) *UI
}

type lockedLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// events are the audit events logged so far, by name.
func (l *lockedLog) events(name string) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.b.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["event"] == name {
			out = append(out, m)
		}
	}
	return out
}

func newBackendPages(t *testing.T) *backendPages {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", dir)
	r, err := root.Init(filepath.Join(dir, "root"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cat, err := catalog.LoadRoot(r.Dir, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	lib, err := library.Open(r, st, cat, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lib.StopImports(5 * time.Second) })
	logs := &lockedLog{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	return &backendPages{root: r, st: st, lib: lib, logs: logs, ui: func(perms ...string) *UI {
		return New(Deps{Library: lib, Catalog: cat, Store: st, RootDir: r.Dir, SignInEnabled: true, Log: log,
			Principal: func(*http.Request) *auth.Principal {
				return &auth.Principal{Kind: "browser-session", ID: "browser:someone", Permissions: perms}
			}})
	}}
}

// A backend names a program to run, so registering one is for whoever is at this computer.
func TestBackendsAreRegisteredFromThisComputerOnly(t *testing.T) {
	p := newBackendPages(t)
	manager := p.ui("models:manage", "models:read", "system:manage", "access:manage")
	definition := `{"schema":"localbase.backend/v1","id":"from-a-page","name":"x","engine":"x","adapter":"process-openai/v1","tasks":["chat"],"formats":["gguf"],` +
		`"capabilities":[],"architectures":{"supported":["x"]},"platforms":[{"os":"darwin","arch":"arm64","accelerator":"metal"}],` +
		`"launch":{"command":"/bin/sh","args":["{port}"],"memory":{"weightsFactor":1,"overheadGb":0,"assumption":"x"}}}`
	if w := adminPost(manager, "/app/backends", url.Values{"definition": {definition}}); w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
		t.Errorf("posting a definition: %d", w.Code)
	}
	if entries, err := os.ReadDir(backends.Dir(p.root.Dir)); err == nil && len(entries) != 0 {
		t.Errorf("a definition was written: %v", entries)
	}
	w := get(manager, "/app/backends")
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, "registered from this computer only") || !strings.Contains(body, "local-base backends add --file") {
		t.Errorf("%d: the page doesn't say how: %.400s", w.Code, body)
	}
	if strings.Contains(body, `hx-post="/app/backends"`) || strings.Contains(body, `name="definition"`) {
		t.Errorf("the page still offers a form: %.400s", body)
	}
	// The command can be copied: the Connect page's copy script, on the command's own scope.
	if !strings.Contains(body, `data-copy-text>local-base backends add --file`) || !strings.Contains(body, `data-copy>Copy</button>`) || !strings.Contains(body, `js/connect.js`) {
		t.Errorf("the command has no copy button: %.600s", body)
	}
}

// Nothing a request reaches can register or remove one. Only the command line does. Each
// directory is read with the packages under it, at least as many as listed: the gateway's are its
// own and every package moved out of it (gatewayPackages in internal/gateway).
func TestOnlyTheCommandLineChangesBackends(t *testing.T) {
	for _, d := range []struct {
		dir      string
		packages int
	}{{".", 1}, {"../gateway", 9}, {"../library", 1}} {
		for _, f := range testutil.GoSources(t, d.dir, d.packages) {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, call := range []string{"backends.Add(", "backends.Remove("} {
				if bytes.Contains(b, []byte(call)) {
					t.Errorf("%s calls %s", f, call)
				}
			}
		}
	}
}

const backendChanged = "audit.model-backend-changed"

// Choosing what runs a model is on record: who, which model, which backend.
func TestChoosingABackendOnAPageIsAudited(t *testing.T) {
	p := newBackendPages(t)
	manager := p.ui("models:manage", "models:read")
	const model = "qwen3.6-35b-a3b-iq4_nl_xl"
	path := "/app/models/backend/" + model
	if w := adminPost(manager, path, url.Values{"backend": {"llama.cpp-cpu"}}); w.Code != http.StatusOK {
		t.Fatalf("set: %d %.200s", w.Code, w.Body)
	}
	if w := adminPost(manager, path, url.Values{"reset": {"1"}}); w.Code != http.StatusOK {
		t.Fatalf("reset: %d %.200s", w.Code, w.Body)
	}
	// One that didn't happen isn't a change, and what it names isn't logged.
	adminPost(manager, path, url.Values{"backend": {"planted-value"}})
	adminPost(p.ui("models:read"), path, url.Values{"backend": {"llama.cpp-cpu"}})
	events := p.logs.events(backendChanged)
	if len(events) != 2 {
		t.Fatalf("%d events: %v", len(events), events)
	}
	for i, want := range []map[string]any{{"action": "set", "backend_id": "llama.cpp-cpu"}, {"action": "reset", "backend_id": ""}} {
		e := events[i]
		if e["action"] != want["action"] || e["backend_id"] != want["backend_id"] || e["actor_kind"] != "browser-session" ||
			e["actor_id"] != "browser:someone" || e["target_kind"] != "model" || e["target_id"] != model {
			t.Errorf("event %d: %v", i, e)
		}
	}
	if strings.Contains(p.logs.b.String(), "planted-value") {
		t.Error("a refused value reached the log")
	}
}

// A UI built without a log still works.
func TestChoosingABackendWithNoLog(t *testing.T) {
	p := newBackendPages(t)
	u := New(Deps{Library: p.lib, Catalog: p.lib.Catalog, Store: p.st, RootDir: p.root.Dir, SignInEnabled: true,
		Principal: func(*http.Request) *auth.Principal {
			return &auth.Principal{Kind: "browser-session", ID: "browser:someone", Permissions: []string{"models:manage", "models:read"}}
		}})
	if w := adminPost(u, "/app/models/backend/qwen3.6-35b-a3b-iq4_nl_xl", url.Values{"backend": {"llama.cpp-cpu"}}); w.Code != http.StatusOK {
		t.Errorf("%d %.200s", w.Code, w.Body)
	}
}

// Choosing a backend while importing counts too. Automatic isn't a choice.
func TestChoosingABackendOnTheImportPageIsAudited(t *testing.T) {
	p := newBackendPages(t)
	if err := p.st.SetSetting(context.Background(), store.PrivateImportsKey, "on"); err != nil {
		t.Fatal(err)
	}
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write(make([]byte, 16<<10)); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		if r.Header.Get("Range") == "bytes=0-0" {
			return
		}
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	t.Cleanup(src.Close)
	t.Cleanup(func() { p.lib.StopImports(5 * time.Second) }) // first, so the source's client leaves
	manager := p.ui("models:manage", "models:read")
	start := func(id, backend string) {
		t.Helper()
		w := adminPost(manager, "/app/models/import/resolve", url.Values{"source": {src.URL + "/m.gguf"}, "allow_http": {"1"}, "id": {id}, "task": {"chat"}})
		token := inputValue(w.Body.String(), "token")
		if token == "" {
			t.Fatalf("no plan: %d %.300s", w.Code, w.Body)
		}
		w = adminPost(manager, "/app/models/import/start", url.Values{"token": {token}, "backend": {backend}, "confirm": {"1"}})
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `id="import-job"`) {
			t.Fatalf("start: %d %.300s", w.Code, w.Body)
		}
	}
	start("chose", "llama.cpp-cpu")
	start("auto", "auto")
	start("none", "")
	events := p.logs.events(backendChanged)
	if len(events) != 1 {
		t.Fatalf("%d events: %v", len(events), events)
	}
	e := events[0]
	if e["action"] != "import" || e["target_id"] != "chose" || e["backend_id"] != "llama.cpp-cpu" || e["actor_id"] != "browser:someone" {
		t.Errorf("%v", e)
	}
}

// inputValue finds a hidden field's value in a page.
func inputValue(page, name string) string {
	_, rest, ok := strings.Cut(page, `name="`+name+`"`)
	if !ok {
		return ""
	}
	_, rest, ok = strings.Cut(rest, `value="`)
	if !ok {
		return ""
	}
	v, _, _ := strings.Cut(rest, `"`)
	return v
}
