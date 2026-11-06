package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/acquire"
	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/library"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// The API keeps imports to public addresses, and offers no way to change that.
func TestImportAPIKeepsToPublicAddresses(t *testing.T) {
	ts, key, st, dir := importAPIGateway(t)
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
	refused := func(name string, body map[string]any, code string) {
		t.Helper()
		status, out := post(body)
		if status != 403 || !strings.Contains(out, code) || strings.Contains(out, "configure") || strings.Contains(out, "allow-") || strings.Contains(out, "127.0.0.1") {
			t.Errorf("%s: %d %s, want 403 %s", name, status, out, code)
		}
	}
	private := map[string]any{"action": "resolve", "source": internal.URL + "/m.gguf", "allowHttp": true, "task": "chat", "id": "x"}
	local := map[string]any{"action": "resolve", "source": file, "task": "chat", "id": "x"}
	metadata := map[string]any{"action": "resolve", "source": "http://169.254.169.254/latest/meta-data/m.gguf", "allowHttp": true, "task": "chat", "id": "x"}
	refused("loopback", private, acquire.CodePrivateAddress)
	refused("confirmed", map[string]any{"action": "resolve", "source": internal.URL + "/m.gguf", "allowHttp": true, "confirmCustomSource": true, "task": "chat", "id": "x"}, acquire.CodePrivateAddress)
	refused("start", map[string]any{"action": "start", "source": internal.URL + "/m.gguf", "allowHttp": true, "confirmCustomSource": true, "task": "chat", "id": "x"}, acquire.CodePrivateAddress)
	refused("metadata", metadata, acquire.CodeAddressRefused)
	refused("local file", local, acquire.CodeLocalPath)
	refused("a missing file", map[string]any{"action": "resolve", "source": "/no/such/file", "task": "chat", "id": "x"}, acquire.CodeLocalPath)
	if reached.Load() != 0 {
		t.Errorf("reached %d times", reached.Load())
	}
	// Allowing local paths does not allow private addresses.
	if err := st.SetSetting(context.Background(), store.LocalPathImportsKey, "on"); err != nil {
		t.Fatal(err)
	}
	refused("loopback, local paths allowed", private, acquire.CodePrivateAddress)
	if status, out := post(local); status != 200 {
		t.Errorf("a local path with its opt-in: %d %s", status, out)
	}
	if err := st.SetSetting(context.Background(), store.LocalPathImportsKey, "off"); err != nil {
		t.Fatal(err)
	}
	// The opt-in comes from the setting, which the CLI on the host writes.
	if err := st.SetSetting(context.Background(), store.PrivateImportsKey, "on"); err != nil {
		t.Fatal(err)
	}
	// Allowing private addresses does not allow local paths, or metadata.
	refused("a local file, private allowed", local, acquire.CodeLocalPath)
	refused("metadata, private allowed", metadata, acquire.CodeAddressRefused)
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

func importAPIGateway(t *testing.T) (*httptest.Server, string, *store.Store, string) {
	t.Helper()
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
	return ts, key, st, dir
}

// Refused private ports look alike: an import cannot tell an open one from a closed
// one, or a closed one from a host that does not exist.
func TestImportRefusalsAreNotAPortOracle(t *testing.T) {
	ts, key, _, _ := importAPIGateway(t)
	open := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer open.Close()
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closed.URL
	closed.Close()
	post := func(action, source string) (int, string) {
		b, _ := json.Marshal(map[string]any{"action": action, "source": source, "allowHttp": true, "confirmCustomSource": true, "task": "chat", "id": "x"})
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
	for _, action := range []string{"resolve", "start"} {
		wantStatus, want := post(action, open.URL+"/m.gguf")
		if wantStatus != 403 {
			t.Fatalf("%s, open port: %d %s", action, wantStatus, want)
		}
		for _, source := range []string{closedURL + "/m.gguf", "http://10.255.255.1:9/m.gguf", "http://192.168.0.1:1/m.gguf", "http://[fc00::1]:9/m.gguf", "http://nas.lan:9/m.gguf"} {
			if status, out := post(action, source); status != wantStatus || out != want {
				t.Errorf("%s %s: %d %s; an open port gets %d %s", action, source, status, out, wantStatus, want)
			}
		}
	}
}

// Nothing a request can reach writes the opt-ins.
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
			for _, word := range []string{"PrivateImportsKey", "LocalPathImportsKey", store.PrivateImportsKey, store.LocalPathImportsKey} {
				if bytes.Contains(b, []byte(word)) {
					t.Errorf("%s mentions an opt-in's setting: %s", f, word)
				}
			}
		}
	}
}

// A person with models:manage can cancel an import through the API.
func TestImportCancelRoute(t *testing.T) {
	ts, key, st, _ := importAPIGateway(t)
	if err := st.SetSetting(context.Background(), store.PrivateImportsKey, "on"); err != nil {
		t.Fatal(err)
	}
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// It sends a little and then holds the download open, so it can't finish before the
		// cancel lands, and a cancel that does nothing fails the test instead of filling the disk.
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
	defer source.Close()
	do := func(method, target, credential string, body any) (int, string) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, ts.URL+target, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+credential)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}
	status, out := do("POST", "/_localbase/model-imports", key, map[string]any{"action": "start", "source": source.URL + "/m.gguf", "allowHttp": true,
		"confirmCustomSource": true, "task": "chat", "id": "x"})
	if status != 202 {
		t.Fatalf("start: %d %s", status, out)
	}
	var started struct{ Job string }
	json.Unmarshal([]byte(out), &started)
	_, chatOnly, err := st.CreateAPIKey(context.Background(), "chat", []string{"inference:chat"})
	if err != nil {
		t.Fatal(err)
	}
	if status, out := do("POST", "/_localbase/model-imports", chatOnly, map[string]any{"action": "cancel", "job": started.Job}); status != 403 {
		t.Errorf("without models:manage: %d %s", status, out)
	}
	if status, out := do("POST", "/_localbase/model-imports", key, map[string]any{"action": "cancel", "job": "imp_none"}); status != 404 {
		t.Errorf("unknown job: %d %s", status, out)
	}
	if status, out := do("POST", "/_localbase/model-imports", key, map[string]any{"action": "cancel", "job": started.Job, "source": "x"}); status != 400 {
		t.Errorf("extra field: %d %s", status, out)
	}
	if status, out := do("POST", "/_localbase/model-imports", key, map[string]any{"action": "cancel", "job": started.Job}); status != 200 {
		t.Errorf("cancel: %d %s", status, out)
	}
	if !waitFor(10*time.Second, func() bool {
		_, out := do("GET", "/_localbase/model-imports?id="+started.Job, key, nil)
		return strings.Contains(out, acquire.CodeCancelled)
	}) {
		t.Error("the job did not end as cancelled")
	}
}

// A source claiming more than the disk can hold is refused before the job starts.
func TestImportNeedingMoreThanTheDiskIsRefused(t *testing.T) {
	ts, key, st, _ := importAPIGateway(t)
	if err := st.SetSetting(context.Background(), store.PrivateImportsKey, "on"); err != nil {
		t.Fatal(err)
	}
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-0/1000000000000000")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte("G"))
	}))
	defer source.Close()
	b, _ := json.Marshal(map[string]any{"action": "start", "source": source.URL + "/m.gguf", "allowHttp": true, "confirmCustomSource": true, "task": "chat", "id": "x"})
	req, _ := http.NewRequest("POST", ts.URL+"/_localbase/model-imports", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusInsufficientStorage || !strings.Contains(string(out), acquire.CodeNoSpace) {
		t.Errorf("%d %s", resp.StatusCode, out)
	}
}
