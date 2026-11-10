package gateway

import (
	"bytes"
	"context"
	"image"
	"image/png"
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
	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// tinyHost has plenty of free memory but a GPU working set too small for any
// model, so every model is refused for good, however idle the machine is.
type tinyHost struct{}

func (tinyHost) Topology() memory.Topology {
	return memory.Topology{Unified: true, SystemCapacity: 64 * memory.GiB, GPUWorkingSetBytes: 1 << 20}
}
func (tinyHost) Snapshot(context.Context) memory.Snapshot {
	return memory.Snapshot{Available: 60 * memory.GiB, Pressure: "normal"}
}

// mediaGateway is a gateway on tinyHost with one media runtime and an installed
// (sparse) model. Nothing can start: the fake binary is only there to be named.
func mediaGateway(t *testing.T, modality, modelID, backendID, binary string) (*httptest.Server, *runtime.Service) {
	t.Helper()
	r, err := root.Init(filepath.Join(scratchDir(t), "root"))
	if err != nil {
		t.Fatal(err)
	}
	cat, _ := catalog.LoadWithFormer()
	reg, _ := backends.Load("")
	spec := cat.ByID(modelID)
	if spec == nil {
		t.Fatalf("no catalog entry %s", modelID)
	}
	dir := r.ModelsDir(modality)
	os.MkdirAll(dir, 0o700)
	installSparse(t, dir, spec)
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &store.Config{Active: map[string]string{modality: modelID}, Selected: map[string][]string{modality: {modelID}}}
	mem := memory.NewController(tinyHost{}, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	mem.SetSampler(nil)
	m := models.FromSpec(spec)
	gate := func(_ context.Context, id string, _ bool) (*runtime.Target, error) {
		if id != modelID {
			return nil, runtime.ErrModelNotFound
		}
		return &runtime.Target{ModelID: id, Model: &m, Variant: &m.Variants[0], Backend: reg.Get(backendID), Dir: dir}, nil
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := runtime.NewService(runtime.ServiceDeps{Modality: modality, Kind: modality, Catalog: cat, Store: st, Memory: mem, Log: log,
		Config: func() *store.Config { return cfg }, Gate: gate, ModelsDir: dir, RootDir: r.Dir, Host: "127.0.0.1",
		Port: freePort(t), Binaries: map[string]string{binary: fakeBinary},
		Verify: func(string, catalog.Artifact, string) error { return nil }})
	t.Cleanup(svc.Close)
	conf := Config{Store: st, Catalog: cat, Auth: &auth.Resolver{Store: st, Mode: "either"}, Log: log, NoAuth: true, MemoryGB: 64, Root: r, Memory: mem}
	switch modality {
	case "image":
		conf.Image = svc
	case "tts":
		conf.TTS = svc
	case "stt":
		conf.STT = svc
	}
	ts := httptest.NewServer(newGateway(t, conf))
	t.Cleanup(ts.Close)
	return ts, svc
}

func wantTooLarge(t *testing.T, name string, status int, code string, header http.Header, body string) {
	t.Helper()
	if status != 422 || code != "model_too_large" || header.Get("Retry-After") != "" {
		t.Errorf("%s: %d %q Retry-After=%q %s", name, status, code, header.Get("Retry-After"), body)
	}
}

func postForm(t *testing.T, ts *httptest.Server, path, contentType string, body io.Reader) result {
	t.Helper()
	resp, err := http.Post(ts.URL+path, contentType, body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return result{resp.StatusCode, resp.Header, string(b)}
}

// Every modality TS answers 422 for: LLM routes (chat, embeddings, decisions),
// image generation and edits, speech and transcription.
func TestModelTooLargeEveryModality(t *testing.T) {
	t.Run("llm", func(t *testing.T) {
		ts, llm, _ := newTestGateway(t, tinyHost{})
		for path, body := range map[string]string{
			"/v1/chat/completions": gatewaytest.ChatBody("hi"),
			"/v1/embeddings":       `{"model":"` + embeddingModel + `","input":"hi"}`,
			"/v1/systemone":        `{"model":"` + decisionModel + `","state":"x","questions":{"q":{"type":"noul"}}}`,
		} {
			r := post(t, ts, path, body)
			wantTooLarge(t, path, r.status, r.code(), r.header, r.body)
		}
		if id, _, _ := llm.Running(); id != "" {
			t.Errorf("%s started", id)
		}
	})
	t.Run("image", func(t *testing.T) {
		if goruntime.GOOS != "darwin" {
			t.Skip("the image runtime target here is Metal")
		}
		ts, svc := mediaGateway(t, "image", imageModel, "stable-diffusion.cpp-metal", "sd-server")
		r := post(t, ts, "/v1/images/generations", `{"prompt":"a lighthouse"}`)
		wantTooLarge(t, "generation", r.status, r.code(), r.header, r.body)
		if svc.Running() != "" {
			t.Errorf("%s started", svc.Running())
		}
	})
	t.Run("image edit", func(t *testing.T) {
		if goruntime.GOOS != "darwin" {
			t.Skip("the image runtime target here is Metal")
		}
		const editModel = "dreamshaper-v8" // the smallest with image-to-image
		ts, svc := mediaGateway(t, "image", editModel, "stable-diffusion.cpp-metal", "sd-server")
		var pic bytes.Buffer
		png.Encode(&pic, image.NewRGBA(image.Rect(0, 0, 8, 8)))
		var form bytes.Buffer
		mw := multipart.NewWriter(&form)
		mw.WriteField("model", editModel)
		mw.WriteField("prompt", "a lighthouse")
		fw, _ := mw.CreateFormFile("image", "a.png")
		fw.Write(pic.Bytes())
		mw.Close()
		r := postForm(t, ts, "/v1/images/edits", mw.FormDataContentType(), &form)
		wantTooLarge(t, "edit", r.status, r.code(), r.header, r.body)
		if svc.Running() != "" {
			t.Errorf("%s started", svc.Running())
		}
	})
	t.Run("speech", func(t *testing.T) {
		ts, svc := mediaGateway(t, "tts", speechModel, "tts-server", "tts-server")
		r := post(t, ts, "/v1/audio/speech", speechBody("hello", ""))
		wantTooLarge(t, "speech", r.status, r.code(), r.header, r.body)
		if svc.Running() != "" {
			t.Errorf("%s started", svc.Running())
		}
	})
	t.Run("transcription", func(t *testing.T) {
		if goruntime.GOOS != "darwin" {
			t.Skip("the whisper backend named here is Metal")
		}
		const whisper = "whisper-base-q8_0"
		ts, svc := mediaGateway(t, "stt", whisper, "whisper.cpp-metal", "whisper-server")
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		mw.WriteField("model", whisper)
		fw, _ := mw.CreateFormFile("file", "a.wav")
		fw.Write([]byte("RIFF"))
		mw.Close()
		r := postForm(t, ts, "/v1/audio/transcriptions", mw.FormDataContentType(), &buf)
		wantTooLarge(t, "transcription", r.status, r.code(), r.header, r.body)
		if svc.Running() != "" {
			t.Errorf("%s started", svc.Running())
		}
	})
}

// Video jobs are the exception, as in TS: the request is accepted and the job
// then fails, and TS's job schema only allows insufficient_memory here. So this
// pins parity, not a preference (DESIGN §13.6). Its message says to retry even
// when that cannot work, in both implementations.
func TestModelTooLargeVideoJobKeepsTSCode(t *testing.T) {
	testVideoHost = tinyHost{}
	g := newVideoGateway(t)
	testVideoHost = nil
	job := g.create(t, "a lighthouse")
	final := g.waitStatus(t, job["id"].(string))
	e, _ := final["error"].(map[string]any)
	if final["status"] != "failed" || e["code"] != "insufficient_memory" {
		t.Errorf("job: %v", final)
	}
	if g.svc.Running() != "" {
		t.Errorf("%s started", g.svc.Running())
	}
}
