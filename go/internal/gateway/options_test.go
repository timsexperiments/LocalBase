package gateway

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/backends"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

const sttModel = "whisper-base-en-q8_0"

// sttBackend is the speech-to-text backend that has a build on this platform.
func sttBackend(reg *backends.Registry) *backends.Definition {
	if goruntime.GOOS == "linux" {
		return reg.Get("whisper.cpp-vulkan")
	}
	return reg.Get("whisper.cpp-metal")
}

// newAuthSTTGateway is a gateway with authentication on, whose transcription
// runtime is the fake whisper-server, and a key that may use it.
func newAuthSTTGateway(t *testing.T) (*httptest.Server, *runtime.Service, string) {
	t.Helper()
	dir := scratchDir(t)
	r, err := root.Init(filepath.Join(dir, "root"))
	if err != nil {
		t.Fatal(err)
	}
	cat, _ := catalog.LoadWithFormer()
	reg, _ := backends.Load("")
	spec := cat.ByID(sttModel)
	modelsDir := r.ModelsDir("stt")
	if err := os.MkdirAll(modelsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	installSparse(t, modelsDir, spec)
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	_, key, err := st.CreateAPIKey(context.Background(), "test", []string{"inference:transcription"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &store.Config{Active: map[string]string{"stt": sttModel}, Selected: map[string][]string{"stt": {sttModel}}}
	mem := memory.NewController(host{}, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	mem.SetSampler(nil)
	m := models.FromSpec(spec)
	gate := func(id string) (*runtime.Target, error) {
		if id != sttModel {
			return nil, runtime.ErrModelNotFound
		}
		return &runtime.Target{ModelID: id, Model: &m, Variant: &m.Variants[0], Backend: sttBackend(reg), Dir: modelsDir}, nil
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := runtime.NewService(runtime.ServiceDeps{Modality: "stt", Kind: "stt", Catalog: cat, Store: st, Memory: mem, Log: log,
		Config: func() *store.Config { return cfg }, Gate: gate, ModelsDir: modelsDir, RootDir: r.Dir, Host: "127.0.0.1",
		Port: freePort(t), Binaries: map[string]string{"whisper-server": fakeBinary},
		Verify: func(string, catalog.Artifact, string) error { return nil }})
	t.Cleanup(svc.Close)
	ts := httptest.NewServer(New(&Server{Store: st, Catalog: cat, Auth: &auth.Resolver{Store: st, Mode: "either"}, Log: log,
		MemoryGB: 64, Root: r, Memory: mem, STT: svc}))
	t.Cleanup(ts.Close)
	return ts, svc, key
}

func transcriptionForm(t *testing.T) (*bytes.Buffer, string) {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	fw, err := w.CreateFormFile("file", "a.wav")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write([]byte("RIFF0000WAVEfmt "))
	w.WriteField("model", sttModel)
	w.Close()
	return &b, w.FormDataContentType()
}

// Outside the browser layer's own paths, OPTIONS is answered before any handler,
// as in TS.
func TestOptionsIsRefusedBeforeAnyHandler(t *testing.T) {
	ts, stt, key := newAuthSTTGateway(t)
	send := func(method, path, credential string) (int, string) {
		t.Helper()
		body, ct := transcriptionForm(t)
		req, _ := http.NewRequest(method, ts.URL+path, body)
		req.Header.Set("Content-Type", ct)
		if credential != "" {
			req.Header.Set("Authorization", "Bearer "+credential)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for _, path := range []string{"/v1/audio/transcriptions", "/v1/audio/translations", "/v1/models", "/v1/chat/completions",
		"/_localbase/models", "/_localbase/runs", "/_localbase/api-keys", "/no/such/route", "/health"} {
		if status, body := send(http.MethodOptions, path, ""); status != http.StatusForbidden {
			t.Errorf("OPTIONS %s: %d %s, want 403", path, status, body)
		}
	}
	if state := stt.Snapshot().State; state != "idle" {
		t.Errorf("a refused request changed the runtime: state %q", state)
	}
	// The fixture does work when authorized.
	if status, body := send(http.MethodPost, "/v1/audio/transcriptions", ""); status != http.StatusUnauthorized {
		t.Errorf("POST without a credential: %d %s, want 401", status, body)
	}
	if status, body := send(http.MethodPost, "/v1/audio/transcriptions", key); status != http.StatusOK {
		t.Errorf("POST with a key: %d %s, want 200", status, body)
	}
}

// guard exempts no method: a route it protects cannot be reached without a
// credential, whatever the request's method.
func TestGuardExemptsNoMethod(t *testing.T) {
	ts, _, _ := newAuthSTTGateway(t)
	srv := ts.Config.Handler.(*Server)
	reached := 0
	guarded := srv.guard("", func(http.ResponseWriter, *http.Request) { reached++ })
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
		http.MethodOptions, http.MethodTrace, http.MethodConnect} {
		w := httptest.NewRecorder()
		guarded(w, httptest.NewRequest(method, "/v1/models", nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", method, w.Code)
		}
	}
	if reached != 0 {
		t.Errorf("the handler ran %d times without a credential", reached)
	}
}
