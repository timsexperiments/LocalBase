package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/library"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// The API keeps imports to public addresses, and offers no way to change that.
func TestImportAPIKeepsToPublicAddresses(t *testing.T) {
	dir := scratchDir(t)
	r, err := root.Init(filepath.Join(dir, "root"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &store.Config{Host: "127.0.0.1", Port: freePort(t), CtxSize: 8192, CtxMode: "auto", Parallel: "1",
		Active: map[string]string{}, Selected: map[string][]string{}}
	cfg.Root, cfg.GatewayHost, cfg.SttHost = r.Dir, "127.0.0.1", "127.0.0.1"
	if err := st.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	_, key, err := st.CreateAPIKey(context.Background(), "manage", []string{"models:manage"})
	if err != nil {
		t.Fatal(err)
	}
	cat, _ := catalog.LoadWithFormer()
	lib, err := library.Open(r, st, cat, nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(New(&Server{Store: st, Root: r, Catalog: cat, Library: lib, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Auth: &auth.Resolver{Store: st, Mode: "either"}}))
	t.Cleanup(ts.Close)
	var reached atomic.Int64
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		w.Header().Set("Content-Range", "bytes 0-0/10")
		w.WriteHeader(http.StatusPartialContent)
	}))
	defer internal.Close()
	post := func(body map[string]any) (int, string) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/_localbase/model-imports", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}
	file := filepath.Join(dir, "private.txt")
	if err := os.WriteFile(file, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		body map[string]any
	}{
		{"loopback", map[string]any{"action": "resolve", "source": internal.URL + "/m.gguf", "allowHttp": true, "task": "chat", "id": "x"}},
		{"confirmed", map[string]any{"action": "resolve", "source": internal.URL + "/m.gguf", "allowHttp": true, "confirmCustomSource": true, "task": "chat", "id": "x"}},
		{"start", map[string]any{"action": "start", "source": internal.URL + "/m.gguf", "allowHttp": true, "confirmCustomSource": true, "task": "chat", "id": "x"}},
		{"metadata", map[string]any{"action": "resolve", "source": "http://169.254.169.254/latest/meta-data/m.gguf", "allowHttp": true, "task": "chat", "id": "x"}},
		{"local file", map[string]any{"action": "resolve", "source": file, "task": "chat", "id": "x"}},
	} {
		status, out := post(c.body)
		if status != 400 || !strings.Contains(out, "import_unresolved") {
			t.Errorf("%s: %d %s", c.name, status, out)
		}
	}
	if reached.Load() != 0 {
		t.Errorf("reached %d times", reached.Load())
	}
	// The opt-in comes from the setting, which the CLI on the host writes.
	if err := st.SetSetting(context.Background(), store.PrivateImportsKey, "on"); err != nil {
		t.Fatal(err)
	}
	status, out := post(map[string]any{"action": "resolve", "source": internal.URL + "/m.gguf", "allowHttp": true, "task": "chat", "id": "x"})
	if status != 200 || reached.Load() != 0 {
		t.Errorf("before confirming: %d %s, reached %d", status, out, reached.Load())
	}
	status, out = post(map[string]any{"action": "resolve", "source": internal.URL + "/m.gguf", "allowHttp": true, "confirmCustomSource": true, "task": "chat", "id": "x"})
	if status != 200 || reached.Load() != 1 {
		t.Errorf("with the opt-in: %d %s, reached %d", status, out, reached.Load())
	}
	// A body cannot ask for it: unknown fields are refused.
	if status, _ := post(map[string]any{"action": "resolve", "source": "hf:org/repo", "allowPrivate": true}); status != 400 {
		t.Errorf("a request set the opt-in: %d", status)
	}
}

// Nothing a request can reach writes the opt-in.
func TestNoRouteSetsTheImportOptIn(t *testing.T) {
	for _, pattern := range []string{"*.go", "../web/*.go"} {
		files, err := filepath.Glob(pattern)
		if err != nil || len(files) == 0 {
			t.Fatalf("%s: %v", pattern, err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(b, []byte("PrivateImportsKey")) || bytes.Contains(b, []byte(store.PrivateImportsKey)) {
				t.Error(fmt.Sprintf("%s mentions the opt-in's setting", f))
			}
		}
	}
}
