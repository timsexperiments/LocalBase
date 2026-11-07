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
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/backends"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// A backend that crashes under a request is a transient failure, and the next
// request starts it again, so the client gets what every transient failure
// gets: 503 with Retry-After (DESIGN §13.6). TS answers 502 upstream_error.

func wantCrashAnswer(t *testing.T, r result) {
	t.Helper()
	if r.status != 503 || r.code() != "service_unavailable" || r.header.Get("Retry-After") == "" {
		t.Errorf("a crash under a request: %d code %v Retry-After %q: %s", r.status, r.code(), r.header.Get("Retry-After"), r.body)
	}
}

func TestLLMCrashAnswers503(t *testing.T) {
	ts, llm, _ := newTestGateway(t, host{})
	gone := func() bool { running, _, _ := llm.Running(); return running == "" }
	for _, c := range []struct{ name, path, body string }{
		{"chat before answering", "/v1/chat/completions", gatewaytest.ChatBody("fake:die")},
		{"chat partway through its answer", "/v1/chat/completions", gatewaytest.ChatBody("fake:die-body")},
		{"embeddings", "/v1/embeddings", `{"model":"` + embeddingModel + `","input":"fake:die"}`},
		{"decision", "/v1/systemone", `{"model":"` + decisionModel + `","state":"fake:die","questions":{"q":{"type":"choice","criteria":{"a":"x","b":"y"}}}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			wantCrashAnswer(t, post(t, ts, c.path, c.body))
			if !gatewaytest.WaitFor(5*time.Second, gone) {
				t.Fatal("the crashed backend was never cleared")
			}
			if r := post(t, ts, "/v1/chat/completions", gatewaytest.ChatBody("hi")); r.status != 200 {
				t.Errorf("the next request: %d %s", r.status, r.body)
			}
		})
	}
}

func TestImageCrashAnswers503(t *testing.T) {
	ts := newImageGateway(t, imageModel)
	for _, prompt := range []string{"fake:die", "fake:die-body"} {
		wantCrashAnswer(t, post(t, ts, "/v1/images/generations", `{"prompt":"`+prompt+`"}`))
		if !gatewaytest.WaitFor(5*time.Second, func() bool { return post(t, ts, "/v1/images/generations", `{"prompt":"a boat"}`).status == 200 }) {
			t.Errorf("the image runtime did not come back after %s", prompt)
		}
	}
}

func TestSpeechCrashAnswers503(t *testing.T) {
	ts := newSpeechGateway(t, io.Discard)
	for _, input := range []string{"fake:die", "fake:die-body"} {
		body := `{"model":"` + speechModel + `","input":"` + input + `","voice":"default"}`
		wantCrashAnswer(t, post(t, ts, "/v1/audio/speech", body))
		ok := `{"model":"` + speechModel + `","input":"good morning","voice":"default"}`
		if !gatewaytest.WaitFor(5*time.Second, func() bool { return post(t, ts, "/v1/audio/speech", ok).status == 200 }) {
			t.Errorf("the speech runtime did not come back after %s", input)
		}
	}
}

// newTranscriptionGateway is a --no-auth gateway whose speech-to-text runtime is
// the fake whisper-server.
func newTranscriptionGateway(t *testing.T) *httptest.Server {
	t.Helper()
	return newTranscriptionGatewayQueue(t, 0)
}

// newTranscriptionGatewayQueue is newTranscriptionGateway with a queue timeout; 0 is the default.
func newTranscriptionGatewayQueue(t *testing.T, queueTimeout time.Duration) *httptest.Server {
	t.Helper()
	if goruntime.GOOS != "darwin" {
		t.Skip("the transcription runtime target here is Metal")
	}
	const sttModel = "whisper-base-q8_0"
	dir := scratchDir(t)
	r, err := root.Init(filepath.Join(dir, "root"))
	if err != nil {
		t.Fatal(err)
	}
	cat, _ := catalog.LoadWithFormer()
	reg, _ := backends.Load("")
	spec := cat.ByID(sttModel)
	modelsDir := r.ModelsDir("stt")
	os.MkdirAll(modelsDir, 0o700)
	installSparse(t, modelsDir, spec)
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &store.Config{Active: map[string]string{"stt": sttModel}, Selected: map[string][]string{"stt": {sttModel}}}
	mem := memory.NewController(host{}, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	mem.SetSampler(nil)
	m := models.FromSpec(spec)
	gate := func(_ context.Context, id string, _ bool) (*runtime.Target, error) {
		if id != sttModel {
			return nil, runtime.ErrModelNotFound
		}
		return &runtime.Target{ModelID: id, Model: &m, Variant: &m.Variants[0], Backend: reg.Get("whisper.cpp-metal"), Dir: modelsDir}, nil
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := runtime.NewService(runtime.ServiceDeps{Modality: "stt", Kind: "stt", Catalog: cat, Store: st, Memory: mem, Log: log,
		Config: func() *store.Config { return cfg }, Gate: gate, ModelsDir: modelsDir, RootDir: r.Dir, Host: "127.0.0.1",
		Port: freePort(t), Binaries: map[string]string{"whisper-server": fakeBinary}, QueueTimeout: queueTimeout,
		Verify: func(string, catalog.Artifact, string) error { return nil }})
	t.Cleanup(svc.Close)
	ts := httptest.NewServer(New(&Server{Store: st, Catalog: cat, Auth: &auth.Resolver{Store: st, Mode: "either"}, Log: log,
		NoAuth: true, MemoryGB: 64, Root: r, Memory: mem, STT: svc}))
	t.Cleanup(ts.Close)
	return ts
}

func TestTranscriptionCrashAnswers503(t *testing.T) {
	ts := newTranscriptionGateway(t)
	send := func(prompt string) result {
		t.Helper()
		var b bytes.Buffer
		mw := multipart.NewWriter(&b)
		mw.WriteField("model", "whisper-base-q8_0")
		mw.WriteField("prompt", prompt)
		fw, _ := mw.CreateFormFile("file", "a.wav")
		fw.Write(testWAV())
		mw.Close()
		resp, err := http.Post(ts.URL+"/v1/audio/transcriptions", mw.FormDataContentType(), &b)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return result{status: resp.StatusCode, header: resp.Header, body: string(body)}
	}
	for _, prompt := range []string{"fake:die", "fake:die-body"} {
		wantCrashAnswer(t, send(prompt))
		if !gatewaytest.WaitFor(5*time.Second, func() bool { return send("the harbor").status == 200 }) {
			t.Errorf("the transcription runtime did not come back after %s", prompt)
		}
	}
}

// testWAV is a short mono 16 kHz silence.
func testWAV() []byte {
	const samples = 8000
	le32 := func(v int) []byte { return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)} }
	var b bytes.Buffer
	b.WriteString("RIFF")
	b.Write(le32(36 + samples*2))
	b.WriteString("WAVEfmt ")
	b.Write(le32(16))
	b.Write([]byte{1, 0, 1, 0})
	b.Write(le32(16000))
	b.Write(le32(32000))
	b.Write([]byte{2, 0, 16, 0})
	b.WriteString("data")
	b.Write(le32(samples * 2))
	b.Write(make([]byte, samples*2))
	return b.Bytes()
}

// A detached run carries the same refusal as its error.
func TestRunCrashCarries503Code(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	code, m := c.Do("POST", "/_localbase/runs/v1/chat/completions", gatewaytest.ChatBody("fake:die"))
	if code != 202 {
		t.Fatalf("create: %d %v", code, m)
	}
	id := m["id"].(string)
	m = c.WaitStatus(id, "failed")
	if e, _ := m["error"].(map[string]any); e == nil || e["code"] != "service_unavailable" {
		t.Errorf("a run whose backend crashed: %v", m)
	}
}

// Over HTTP: a load longer than the queue's deadline answers the request, not 429.
func TestATranscriptionLongerToLoadThanTheQueueTimeoutIsAnswered(t *testing.T) {
	t.Setenv("FAKE_STARTUP_MS", "900")
	ts := newTranscriptionGatewayQueue(t, 150*time.Millisecond)
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	_ = mw.WriteField("model", "whisper-base-q8_0")
	fw, err := mw.CreateFormFile("file", "a.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(testWAV()); err != nil {
		t.Fatal(err)
	}
	_ = mw.Close()
	start := time.Now()
	resp, err := http.Post(ts.URL+"/v1/audio/transcriptions", mw.FormDataContentType(), &b)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Errorf("after %s: %d %s", time.Since(start).Round(time.Millisecond), resp.StatusCode, body)
	}
	if took := time.Since(start); took < 900*time.Millisecond {
		t.Errorf("took %s, so the load wasn't longer than the queue's deadline", took)
	}
}
