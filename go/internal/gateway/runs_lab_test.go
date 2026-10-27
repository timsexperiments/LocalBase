package gateway

import (
	"bytes"
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
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/backends"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

const (
	imageModel = "z-image-turbo-q4_0"
	editModel  = "flux2-klein-4b-q4_0" // supports image-to-image edits
)

// newImageGateway is a --no-auth gateway whose image runtime is the fake
// sd-server.
func newImageGateway(t *testing.T, imageModel string) *httptest.Server {
	t.Helper()
	if goruntime.GOOS != "darwin" {
		t.Skip("the image runtime target here is Metal")
	}
	dir := t.TempDir()
	r, err := root.Init(filepath.Join(dir, "root"))
	if err != nil {
		t.Fatal(err)
	}
	cat, _ := catalog.LoadWithFormer()
	reg, _ := backends.Load("")
	spec := cat.ByID(imageModel)
	modelsDir := r.ModelsDir("image")
	os.MkdirAll(modelsDir, 0o700)
	installSparse(t, modelsDir, spec)
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &store.Config{Active: map[string]string{"image": imageModel}, Selected: map[string][]string{"image": {imageModel}}}
	mem := memory.NewController(host{}, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	mem.SetSampler(nil)
	m := models.FromSpec(spec)
	sd := reg.Get("stable-diffusion.cpp-metal")
	gate := func(id string) (*runtime.Target, error) {
		if id != imageModel {
			return nil, runtime.ErrModelNotFound
		}
		return &runtime.Target{ModelID: id, Model: &m, Variant: &m.Variants[0], Backend: sd, Dir: modelsDir}, nil
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := runtime.NewService(runtime.ServiceDeps{Modality: "image", Kind: "image", Catalog: cat, Store: st, Memory: mem, Log: log,
		Config: func() *store.Config { return cfg }, Gate: gate, ModelsDir: modelsDir, RootDir: r.Dir, Host: "127.0.0.1",
		Port: freePort(t), Binaries: map[string]string{"sd-server": fakeBinary},
		Verify: func(string, catalog.Artifact, string) error { return nil }})
	t.Cleanup(svc.Close)
	srv := New(&Server{Store: st, Catalog: cat, Auth: &auth.Resolver{Store: st, Mode: "either"}, Log: log,
		NoAuth: true, MemoryGB: 64, Root: r, Memory: mem, Image: svc})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

// An image run survives its page, keeps its result in memory only, and
// serves it from /result.
func TestRunImage(t *testing.T) {
	ts := newImageGateway(t, imageModel)
	c := runClient{t: t, base: ts.URL}
	code, m := c.do("POST", "/_localbase/runs/v1/images/generations", `{"prompt":"a lighthouse fake:slow-image"}`)
	if code != 202 || m["label"] != "Image" {
		t.Fatalf("create: %d %v", code, m)
	}
	id := m["id"].(string)
	if code, e := c.do("GET", "/_localbase/runs/"+id+"/result", ""); code != 409 || e["error"].(map[string]any)["code"] != "run_result_unavailable" {
		t.Errorf("result before completion: %d %v", code, e)
	}
	m = c.waitStatus(id, "completed")
	res, _ := m["result"].(map[string]any)
	if res == nil || res["contentType"] != "application/json" || res["url"] != "/_localbase/runs/"+id+"/result" || m["output"] != nil {
		t.Fatalf("run: %v", m)
	}
	resp, err := http.Get(ts.URL + "/_localbase/runs/" + id + "/result")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"b64_json"`) || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("result: %d %s", resp.StatusCode, b)
	}
	// The checkpoint never holds the media result.
	cp, _ := os.ReadFile(filepath.Join(runServer(ts).Root.Dir, "runs", id+".json"))
	if len(cp) == 0 || strings.Contains(string(cp), "b64_json") {
		t.Errorf("checkpoint: %s", cp)
	}
	// After a restart the result is gone, and the run says so.
	s := runServer(ts)
	b2 := httptest.NewServer(New(&Server{Store: s.Store, Catalog: s.Catalog, Auth: s.Auth, Log: s.Log, NoAuth: true, Root: s.Root, Memory: s.Memory, Image: s.Image}))
	defer b2.Close()
	if after := (runClient{t: t, base: b2.URL}).get(id); after["status"] != "interrupted" || after["reason"] != "gateway_restart" || after["result"] != nil {
		t.Errorf("after a restart: %v", after)
	}
}

// Stop on an image run cancels the backend request.
func TestRunImageCancel(t *testing.T) {
	cancelLog := filepath.Join(t.TempDir(), "cancel.log")
	t.Setenv("FAKE_CANCEL_LOG", cancelLog)
	ts := newImageGateway(t, imageModel)
	c := runClient{t: t, base: ts.URL}
	_, m := c.do("POST", "/_localbase/runs/v1/images/generations", `{"prompt":"fake:slow-image"}`)
	id := m["id"].(string)
	c.waitStatus(id, "in_progress")
	time.Sleep(200 * time.Millisecond) // the backend has the request
	if code, m := c.do("POST", "/_localbase/runs/"+id+"/cancel", ""); code != 200 || m["status"] != "cancelled" {
		t.Fatalf("stop: %d %v", code, m)
	}
	if !waitFor(5*time.Second, func() bool { b, _ := os.ReadFile(cancelLog); return strings.Contains(string(b), "cancelled") }) {
		t.Error("the image backend kept going after Stop")
	}
}

// Multipart bodies (an image edit) pass through unchanged: refusals before
// the queue are byte-for-byte /v1's.
func TestRunImageEditMultipart(t *testing.T) {
	ts := newImageGateway(t, editModel)
	c := runClient{t: t, base: ts.URL}
	form := func(withImage bool) (string, []byte) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		mw.WriteField("model", editModel)
		mw.WriteField("prompt", "make it blue")
		if withImage {
			fw, _ := mw.CreateFormFile("image", "in.png")
			fw.Write(tinyPNG(t))
		}
		mw.Close()
		return mw.FormDataContentType(), buf.Bytes()
	}
	send := func(path, ctype string, body []byte) (int, string) {
		resp, err := http.Post(ts.URL+path, ctype, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for _, withImage := range []bool{false, true} {
		ct, body := form(withImage)
		v1, v1Body := send("/v1/images/edits", ct, body)
		code, runBody := send("/_localbase/runs/v1/images/edits", ct, body)
		if code != v1 || code != 400 || runBody != v1Body {
			t.Errorf("image %v: run %d %s, /v1 %d %s", withImage, code, runBody, v1, v1Body)
		}
	}
	if _, m := c.do("GET", "/_localbase/runs", ""); len(m["data"].([]any)) != 0 {
		t.Errorf("refused edits left runs: %v", m)
	}
}

// The indicator's model comes from a JSON body or a multipart field.
func TestRunRequestModel(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "a.wav")
	fw.Write([]byte("RIFF"))
	mw.WriteField("model", "whisper-base-q8_0")
	mw.Close()
	if got := requestModel(mw.FormDataContentType(), buf.Bytes()); got != "whisper-base-q8_0" {
		t.Errorf("multipart: %q", got)
	}
	if got := requestModel("application/json", []byte(`{"model":"m1","input":"x"}`)); got != "m1" {
		t.Errorf("json: %q", got)
	}
	if got := requestModel("multipart/form-data; boundary=x", []byte("junk")); got != "" {
		t.Errorf("junk: %q", got)
	}
}

// Speech and transcription are run targets; with no runtime configured
// they are refused before the queue exactly as /v1 refuses them.
func TestRunMediaTargetsNotConfigured(t *testing.T) {
	ts := newTestServer(t)
	c := runClient{t: t, base: ts.URL}
	for _, target := range []string{"/v1/audio/speech", "/v1/images/generations"} {
		body := `{"model":"x","input":"hi","voice":"a","prompt":"p"}`
		v1 := post(t, ts, target, body)
		code, m := c.do("POST", "/_localbase/runs"+target, body)
		if code != v1.status || m["error"] == nil {
			t.Errorf("%s: run %d %v, /v1 %d %s", target, code, m, v1.status, v1.body)
		}
	}
	if _, m := c.do("GET", "/_localbase/runs", ""); len(m["data"].([]any)) != 0 {
		t.Errorf("refused requests left runs: %v", m)
	}
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
