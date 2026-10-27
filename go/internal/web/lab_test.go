package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

type fakeAPI struct {
	method, path, ctype string
	body                []byte
	reply               APIResult
	calls               []string // "METHOD path content-type"
}

func (f *fakeAPI) call(_ context.Context, _ *auth.Principal, method, path, ctype string, body []byte) APIResult {
	f.method, f.path, f.ctype, f.body = method, path, ctype, body
	f.calls = append(f.calls, method+" "+path+" "+ctype)
	return f.reply
}

func htmxPost(path string, form url.Values) *http.Request {
	r := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("HX-Request", "true")
	r.Header.Set("X-Localbase-UI", "1")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	withCSRF(&UI{}, r) // rootless UIs share one key
	return r
}

func TestLabSpeechCallsTheAPIAndServesTheResult(t *testing.T) {
	api := &fakeAPI{reply: APIResult{Status: 200, Body: []byte("RIFFwav")}}
	u := New(Deps{NoAuth: true, Call: api.call})
	w := httptest.NewRecorder()
	u.ServeHTTP(w, htmxPost("/app/lab/speech", url.Values{"model": {"m"}, "input": {"hi"}, "voice": {"af_heart"}}))
	if w.Code != 200 || api.path != "/v1/audio/speech" || !strings.Contains(string(api.body), `"voice":"af_heart"`) {
		t.Fatalf("%d %s %s", w.Code, api.path, api.body)
	}
	html := w.Body.String()
	i := strings.Index(html, "/app/lab/results/")
	if i < 0 || !strings.Contains(html, `id="lab-result"`) || strings.Contains(html, "<html") {
		t.Fatalf("fragment: %s", html)
	}
	src := html[i : i+len("/app/lab/results/")+32]
	w = httptest.NewRecorder()
	u.ServeHTTP(w, httptest.NewRequest("GET", src, nil))
	if w.Code != 200 || w.Body.String() != "RIFFwav" || w.Header().Get("Content-Type") != "audio/wav" {
		t.Fatalf("result: %d %q", w.Code, w.Body.String())
	}
	// Another person cannot fetch it.
	id := strings.TrimPrefix(src, "/app/lab/results/")
	if u.results.get("someone-else", id) != nil {
		t.Fatal("result not bound to its owner")
	}
}

func TestLabShowsAPIErrors(t *testing.T) {
	api := &fakeAPI{reply: APIResult{Status: 400, Body: []byte(`{"error":{"message":"Voice \"x\" is not supported <b>.","code":"voice_not_supported"}}`)}}
	u := New(Deps{NoAuth: true, Call: api.call})
	w := httptest.NewRecorder()
	u.ServeHTTP(w, htmxPost("/app/lab/speech", url.Values{"model": {"m"}, "input": {"hi"}, "voice": {"x"}}))
	if !strings.Contains(w.Body.String(), `role="alert"`) || !strings.Contains(w.Body.String(), "&lt;b&gt;") {
		t.Fatalf("error not shown escaped: %s", w.Body.String())
	}
	w = httptest.NewRecorder()
	u.ServeHTTP(w, htmxPost("/app/lab/image", url.Values{"prompt": {"a"}, "seed": {"1.5"}}))
	if !strings.Contains(w.Body.String(), "seed must be a whole number.") || api.path != "/v1/audio/speech" {
		t.Fatalf("seed check: %s (called %s)", w.Body.String(), api.path)
	}
}

func TestLabRequiresUIRequests(t *testing.T) {
	api := &fakeAPI{reply: APIResult{Status: 200}}
	u := New(Deps{NoAuth: true, Call: api.call})
	for _, path := range []string{"/app/lab/speech", "/app/lab/transcribe", "/app/lab/video", "/app/lab/video/6f1c2c3e-8a7b-4c1d-9e2f-0123456789ab/cancel", "/app/voices", "/app/voices/voice-0123456789ab/delete"} {
		r := htmxPost(path, url.Values{})
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		w := httptest.NewRecorder()
		u.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden || api.path != "" {
			t.Errorf("%s cross-site: %d, called %q", path, w.Code, api.path)
		}
	}
}

func TestVoicesPage(t *testing.T) {
	list := `{"object":"list","data":[{"id":"voice-0123456789ab","name":"Narrator <i>","durationMs":4000,"sizeBytes":10,"sha256":"x","createdAt":"2026-10-20T10:00:00.000Z","consent":{"statement":"voice-consent-v1","attestedAt":"2026-10-20T10:00:00.000Z"}}],
		"canManage":%s,"limits":{"minSeconds":3,"maxSeconds":30,"maxUploadBytes":1,"maxVoices":32},"consentStatement":"I have the right."}`
	for _, manage := range []string{"true", "false"} {
		api := &fakeAPI{reply: APIResult{Status: 200, Body: []byte(strings.Replace(list, "%s", manage, 1))}}
		u := New(Deps{NoAuth: true, Call: api.call})
		w := httptest.NewRecorder()
		u.ServeHTTP(w, httptest.NewRequest("GET", "/app/voices", nil))
		html := w.Body.String()
		if w.Code != 200 || !strings.Contains(html, "Narrator &lt;i&gt;") || !strings.Contains(html, "4.0 s") {
			t.Fatalf("%s: %d %s", manage, w.Code, html)
		}
		form, audio := strings.Contains(html, "Add a voice"), strings.Contains(html, "/app/voices/voice-0123456789ab/audio")
		if (manage == "true") != form || (manage == "true") != audio {
			t.Errorf("canManage=%s: form %v, audio %v", manage, form, audio)
		}
	}
	// Uploads pass through unchanged, with their multipart boundary.
	api := &fakeAPI{reply: APIResult{Status: 201, Body: []byte(`{"id":"voice-0123456789ab","name":"N"}`)}}
	u := New(Deps{NoAuth: true, Call: api.call})
	r := httptest.NewRequest("POST", "/app/voices", strings.NewReader("--b\r\n\r\n--b--"))
	r.Header.Set("Content-Type", "multipart/form-data; boundary=b")
	r.Header.Set("HX-Request", "true")
	r.Header.Set("X-Localbase-UI", "1")
	withCSRF(u, r)
	w := httptest.NewRecorder()
	u.ServeHTTP(w, r)
	if len(api.calls) != 2 || api.calls[0] != "POST /_localbase/voices multipart/form-data; boundary=b" ||
		api.calls[1] != "GET /_localbase/voices " || !strings.Contains(w.Body.String(), "Added N.") {
		t.Fatalf("create: %q %s", api.calls, w.Body.String())
	}
}

// TestLabVideo covers the Lab's video section: text and image-to-video
// models are offered (speech to video is not), the starting picture input
// appears for image-to-video models only, a job is created at the model's
// profile with the picture as input_reference, the page polls and can
// cancel, and the finished video is served with byte ranges.
func TestLabVideo(t *testing.T) {
	cat, err := catalog.LoadWithFormer()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &store.Config{Active: map[string]string{"video": "wan2.1-t2v-1.3b-q8_0"},
		Selected: map[string][]string{"video": {"wan2.1-t2v-1.3b-q8_0", "fastwan2.2-ti2v-5b-q6_k", "wan2.2-s2v-14b-fp8"}}}
	llm := runtime.NewLLM(runtime.LLMDeps{Catalog: cat, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, cfg)
	api := &fakeAPI{}
	u := New(Deps{NoAuth: true, Call: api.call, Catalog: cat, LLM: llm})

	w := httptest.NewRecorder()
	u.ServeHTTP(w, httptest.NewRequest("GET", "/app/lab?tab=video", nil))
	page := w.Body.String()
	if !strings.Contains(page, `value="wan2.1-t2v-1.3b-q8_0"`) || !strings.Contains(page, `value="fastwan2.2-ti2v-5b-q6_k"`) ||
		strings.Contains(page, `value="wan2.2-s2v-14b-fp8"`) {
		t.Fatalf("models: %s", page)
	}
	if !strings.Contains(page, "Makes 33 frames at 16 fps, 320×320") || strings.Contains(page, "data-video-source") {
		t.Fatalf("text-to-video options: %s", page)
	}
	w = httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/app/lab/video/options?model=fastwan2.2-ti2v-5b-q6_k", nil)
	r.Header.Set("HX-Request", "true")
	r.Header.Set("HX-Target", "lab-video-options")
	u.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `data-width="480" data-height="832"`) || strings.Contains(w.Body.String(), "<html") {
		t.Fatalf("image-to-video options: %s", w.Body.String())
	}

	// Create with a starting picture.
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	mw.WriteField("model", "fastwan2.2-ti2v-5b-q6_k")
	mw.WriteField("prompt", "a boat")
	fw, _ := mw.CreateFormFile("input_reference", "start.png")
	fw.Write([]byte("\x89PNGfake"))
	mw.Close()
	r = httptest.NewRequest("POST", "/app/lab/video", &b)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("HX-Request", "true")
	r.Header.Set("X-Localbase-UI", "1")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	withCSRF(&UI{}, r)
	const id = "6f1c2c3e-8a7b-4c1d-9e2f-0123456789ab"
	api.reply = APIResult{Status: 202, Body: []byte(`{"object":"localbase.video.job","id":"` + id + `","status":"queued","created_at":1}`)}
	w = httptest.NewRecorder()
	u.ServeHTTP(w, r)
	var sent map[string]any
	json.Unmarshal(api.body, &sent)
	if api.path != "/v1/videos" || sent["width"] != float64(480) || sent["height"] != float64(832) || sent["frames"] != float64(81) ||
		sent["fps"] != float64(16) || sent["input_reference"] != base64.StdEncoding.EncodeToString([]byte("\x89PNGfake")) {
		t.Fatalf("request %s %v", api.path, sent)
	}
	frag := w.Body.String()
	if !strings.Contains(frag, `hx-get="/app/lab/video/`+id+`"`) || !strings.Contains(frag, `hx-trigger="every 2s"`) ||
		!strings.Contains(frag, "/cancel") {
		t.Fatalf("queued fragment: %s", frag)
	}

	// Completed: the video plays, with byte ranges.
	api.reply = APIResult{Status: 200, Body: []byte(`{"object":"localbase.video.job","id":"` + id + `","status":"completed","created_at":1,"completed_at":2,"content_type":"video/mp4","bytes":10,"fps":16,"frames":81}`)}
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/app/lab/video/"+id, nil)
	r.Header.Set("HX-Request", "true")
	u.ServeHTTP(w, r)
	if frag := w.Body.String(); !strings.Contains(frag, `<video controls playsinline src="/app/lab/video/`+id+`/content"`) ||
		strings.Contains(frag, "every 2s") {
		t.Fatalf("completed fragment: %s", frag)
	}
	api.reply = APIResult{Status: 200, Header: http.Header{"Content-Type": {"video/mp4"}}, Body: []byte("0123456789")}
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/app/lab/video/"+id+"/content", nil)
	r.Header.Set("Range", "bytes=2-5")
	u.ServeHTTP(w, r)
	if w.Code != http.StatusPartialContent || w.Body.String() != "2345" || w.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("content: %d %q %v", w.Code, w.Body.String(), w.Header())
	}

	// Failed jobs show the API's message.
	api.reply = APIResult{Status: 200, Body: []byte(`{"object":"localbase.video.job","id":"` + id + `","status":"failed","created_at":1,"completed_at":2,"error":{"code":"insufficient_memory","message":"Not enough free memory."}}`)}
	w = httptest.NewRecorder()
	u.ServeHTTP(w, httptest.NewRequest("GET", "/app/lab/video/"+id, nil))
	if !strings.Contains(w.Body.String(), "Not enough free memory.") {
		t.Fatalf("failed: %s", w.Body.String())
	}
}

// My voices: Add opens a sheet that closes on success; each voice has a
// Play button and an options sheet (Use in the Lab, Rename, Delete); without
// models:manage there is none of that, only the names.
func TestVoicesSheets(t *testing.T) {
	list := `{"object":"list","data":[{"id":"voice-0123456789ab","name":"Narrator","durationMs":4000,"createdAt":"2026-10-20T10:00:00.000Z"}],
		"canManage":%s,"limits":{"minSeconds":3,"maxSeconds":30},"consentStatement":"I have the right."}`
	api := &fakeAPI{reply: APIResult{Status: 200, Body: []byte(strings.Replace(list, "%s", "true", 1))}}
	html := get(New(Deps{NoAuth: true, Call: api.call}), "/app/voices").Body.String()
	for _, want := range []string{`data-open-sheet="voice-add" aria-haspopup="dialog" aria-label="Add a voice"`, `<dialog id="voice-add" class="sheet`,
		`data-close-sheet-on-success`, `data-play="/app/voices/voice-0123456789ab/audio" aria-label="Play Narrator" aria-pressed="false"`,
		`<dialog id="voice-voice-0123456789ab" class="sheet`, `href="/app/lab?tab=speech">Use in the Lab</a>`, "Delete voice", "js/voices.js"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	api.reply.Body = []byte(strings.Replace(list, "%s", "false", 1))
	html = get(New(Deps{NoAuth: true, Call: api.call}), "/app/voices").Body.String()
	if strings.Contains(html, "data-play") || strings.Contains(html, `id="voice-add"`) || strings.Contains(html, "Delete voice") || !strings.Contains(html, "Narrator") {
		t.Errorf("read-only page offers management")
	}
}
