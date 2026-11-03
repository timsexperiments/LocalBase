package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

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
	html := waitSpeech(t, u, w)
	if api.path != "/v1/audio/speech" || !strings.Contains(string(api.body), `"voice":"af_heart"`) {
		t.Fatalf("%s %s", api.path, api.body)
	}
	i := strings.Index(html, "/app/lab/results/")
	if i < 0 || strings.Contains(html, "<html") {
		t.Fatalf("fragment: %s", html)
	}
	path := html[i : i+len("/app/lab/results/")+32]
	w = httptest.NewRecorder()
	u.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	if w.Code != 200 || w.Body.String() != "RIFFwav" {
		t.Fatalf("result: %d %q", w.Code, w.Body.String())
	}
}

// waitSpeech follows a script's tray item until it ends.
func waitSpeech(t *testing.T, u *UI, started *httptest.ResponseRecorder) string {
	t.Helper()
	m := regexp.MustCompile(`id="run-(sp[0-9a-f]+)"`).FindStringSubmatch(started.Body.String())
	if m == nil {
		t.Fatalf("no tray item: %s", started.Body.String())
	}
	for i := 0; i < 500; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/app/lab/speech/"+m[1], nil)
		r.Header.Set("HX-Request", "true")
		u.ServeHTTP(w, r)
		if !strings.Contains(w.Body.String(), "data-poll") {
			return w.Body.String()
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the script never ended")
	return ""
}

func TestLabShowsAPIErrors(t *testing.T) {
	api := &fakeAPI{reply: APIResult{Status: 400, Body: []byte(`{"error":{"message":"Voice \"x\" is not supported <b>.","code":"voice_not_supported"}}`)}}
	u := New(Deps{NoAuth: true, Call: api.call})
	w := httptest.NewRecorder()
	u.ServeHTTP(w, htmxPost("/app/lab/speech", url.Values{"model": {"m"}, "input": {"hi"}, "voice": {"x"}}))
	if html := waitSpeech(t, u, w); !strings.Contains(html, `role="alert"`) || !strings.Contains(html, "&lt;b&gt;") {
		t.Fatalf("error not shown escaped: %s", html)
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

// TestLabVideo covers the Video studio: text and image-to-video
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
	u := New(Deps{NoAuth: true, Call: api.call, Catalog: cat, LLM: llm, RootDir: installedRoot(t, cat, "wan2.1-t2v-1.3b-q8_0", "fastwan2.2-ti2v-5b-q6_k", "wan2.2-s2v-14b-fp8")})

	w := httptest.NewRecorder()
	u.ServeHTTP(w, httptest.NewRequest("GET", "/app/studio/video", nil))
	page := w.Body.String()
	if !strings.Contains(page, `value="wan2.1-t2v-1.3b-q8_0"`) || !strings.Contains(page, `value="fastwan2.2-ti2v-5b-q6_k"`) ||
		strings.Contains(page, `value="wan2.2-s2v-14b-fp8"`) {
		t.Fatalf("models: %s", page)
	}
	if !strings.Contains(page, "About 2 seconds, square. It can take up to") || strings.Contains(page, "data-video-source") {
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
	withCSRF(u, r)
	const id = "6f1c2c3e-8a7b-4c1d-9e2f-0123456789ab"
	api.reply = APIResult{Status: 202, Body: []byte(`{"object":"localbase.video.job","id":"` + id + `","status":"queued","created_at":` + strconv.FormatInt(time.Now().Unix(), 10) + `}`)}
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
		!strings.Contains(frag, "/cancel") || !strings.Contains(frag, "Waiting for its turn") || !strings.Contains(frag, "it keeps going") ||
		!strings.Contains(frag, "video-tall") || !strings.Contains(frag, "a boat") || strings.Contains(frag, "%") {
		t.Fatalf("queued fragment: %s", frag)
	}
	if w.Header().Get("HX-Retarget") != "#lab-items" {
		t.Fatalf("a job joins the tray: %v", w.Header())
	}
	// It is still there after leaving and coming back, and the indicator counts it.
	api.reply = APIResult{Status: 200, Body: []byte(`{"object":"localbase.video.job","id":"` + id + `","status":"in_progress","created_at":1}`)}
	w = httptest.NewRecorder()
	u.ServeHTTP(w, httptest.NewRequest("GET", "/app/studio/video", nil))
	// (Started long ago, so past half the model's limit: it says so.)
	if !strings.Contains(w.Body.String(), "Making the video") || !strings.Contains(w.Body.String(), `id="run-`+id+`"`) || !strings.Contains(w.Body.String(), "taking a while") {
		t.Fatalf("the tray lost the job: %s", w.Body.String())
	}
	w = httptest.NewRecorder()
	u.ServeHTTP(w, httptest.NewRequest("GET", "/app/lab/active", nil))
	if !strings.Contains(w.Body.String(), `"video":1`) {
		t.Fatalf("active: %s", w.Body.String())
	}

	// Completed: the video plays, with byte ranges.
	api.reply = APIResult{Status: 200, Body: []byte(`{"object":"localbase.video.job","id":"` + id + `","status":"completed","created_at":1,"completed_at":2,"content_type":"video/mp4","bytes":10,"fps":16,"frames":81}`)}
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/app/lab/video/"+id, nil)
	r.Header.Set("HX-Request", "true")
	u.ServeHTTP(w, r)
	if frag := w.Body.String(); !strings.Contains(frag, `<video controls playsinline preload="metadata" src="/app/lab/video/`+id+`/content"`) ||
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
	if !strings.Contains(w.Body.String(), "isn’t enough free memory") || !strings.Contains(w.Body.String(), "Try again") ||
		!strings.Contains(w.Body.String(), "/app/models/running") {
		t.Fatalf("failed: %s", w.Body.String())
	}

	// Try again sends the same words, model and picture.
	api.reply = APIResult{Status: 202, Body: []byte(`{"object":"localbase.video.job","id":"7f1c2c3e-8a7b-4c1d-9e2f-0123456789ab","status":"queued","created_at":2}`)}
	r = htmxPost("/app/lab/video/"+id+"/again", url.Values{})
	withCSRF(u, r)
	w = httptest.NewRecorder()
	u.ServeHTTP(w, r)
	sent = nil
	json.Unmarshal(api.body, &sent)
	if api.path != "/v1/videos" || sent["prompt"] != "a boat" || sent["model"] != "fastwan2.2-ti2v-5b-q6_k" ||
		sent["input_reference"] != base64.StdEncoding.EncodeToString([]byte("\x89PNGfake")) {
		t.Fatalf("again: %s %v (%s)", api.path, sent, w.Body.String())
	}

	// Only one runs at a time: it says what to do.
	api.reply = APIResult{Status: 429, Body: []byte(`{"error":{"code":"video_job_busy","message":"A local video job is already in progress."}}`)}
	r = htmxPost("/app/lab/video/"+id+"/again", url.Values{})
	withCSRF(u, r)
	w = httptest.NewRecorder()
	u.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "One video at a time") || strings.Contains(w.Body.String(), "already in progress") {
		t.Fatalf("busy: %s", w.Body.String())
	}

	// Remove deletes the job; one the API has forgotten leaves the tray.
	r = htmxPost("/app/lab/video/"+id+"/remove", url.Values{})
	withCSRF(u, r)
	u.ServeHTTP(httptest.NewRecorder(), r)
	if api.method != "DELETE" || api.path != "/v1/videos/"+id {
		t.Fatalf("remove: %s %s", api.method, api.path)
	}
	api.reply = APIResult{Status: 404, Body: []byte(`{"error":{"message":"gone"}}`)}
	w = httptest.NewRecorder()
	u.ServeHTTP(w, httptest.NewRequest("GET", "/app/studio/video", nil))
	if strings.Contains(w.Body.String(), "id=\"run-") {
		t.Fatalf("a forgotten job stayed: %s", w.Body.String())
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
		`<dialog id="voice-voice-0123456789ab" class="sheet`, `href="/app/studio/speech">Use in Speech</a>`, "Delete voice", "js/voices.js"} {
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

// A blank seed in the Lab is a random one, sent explicitly; a seed the person
// chose goes through as it is.
func TestLabImageSeed(t *testing.T) {
	api := &fakeAPI{reply: APIResult{Status: 200, Body: []byte(`{"data":[{"b64_json":"cG5n"}]}`)}}
	u := New(Deps{NoAuth: true, Call: api.call})
	send := func(form url.Values) int64 {
		t.Helper()
		w := httptest.NewRecorder()
		u.ServeHTTP(w, htmxPost("/app/lab/image", form))
		var req struct {
			Seed *int64 `json:"seed"`
		}
		if err := json.Unmarshal(api.body, &req); err != nil || req.Seed == nil {
			t.Fatalf("request %s: %v", api.body, err)
		}
		return *req.Seed
	}
	if got := send(url.Values{"prompt": {"a"}, "seed": {"7"}}); got != 7 {
		t.Errorf("chosen seed: %d", got)
	}
	seen := map[int64]bool{}
	for range 8 {
		got := send(url.Values{"prompt": {"a"}, "seed": {" "}})
		if got < 0 || got > 2147483647 {
			t.Errorf("seed %d is out of the API's range", got)
		}
		seen[got] = true
	}
	if len(seen) < 2 {
		t.Errorf("blank seeds repeat: %v", seen)
	}
}

// Detect has to ask for it: with no language, the runtime takes the speech as English.
func TestTranscribeDetectsLanguage(t *testing.T) {
	html, err := templateFS.ReadFile("templates/lab.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), `name="language" value="auto" data-label="Detect language" checked`) {
		t.Error("Detect does not send language=auto")
	}
}

// An English-only model offers no language and no translation.
func TestEnglishOnlyModel(t *testing.T) {
	cat, _ := catalog.Load("")
	u := New(Deps{NoAuth: true, Catalog: cat})
	for id, want := range map[string]bool{"whisper-tiny.en-q8_0": true, "whisper-base.en": true, "whisper-base-q8_0": false, "whisper-large-v3-turbo": false, "english-thing": false} {
		if got := u.englishOnly(id); got != want {
			t.Errorf("%s: %v", id, got)
		}
	}
}

// The Transcribe page says a language: the person's, else detect.
func TestTranscribeSendsLanguage(t *testing.T) {
	for chosen, want := range map[string]string{"": "auto", "auto": "auto", "es": "es"} {
		api := &fakeAPI{reply: APIResult{Status: 200, Body: []byte(`{"text":"hi"}`)}}
		u := New(Deps{NoAuth: true, Call: api.call})
		var b bytes.Buffer
		mw := multipart.NewWriter(&b)
		fw, _ := mw.CreateFormFile("file", "a.wav")
		fw.Write([]byte("RIFF"))
		if chosen != "" {
			mw.WriteField("language", chosen)
		}
		mw.Close()
		r := httptest.NewRequest("POST", "/app/lab/transcribe", &b)
		r.Header.Set("Content-Type", mw.FormDataContentType())
		r.Header.Set("HX-Request", "true")
		r.Header.Set("X-Localbase-UI", "1")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		withCSRF(&UI{}, r)
		u.ServeHTTP(httptest.NewRecorder(), r)
		if !strings.Contains(string(api.body), "name=\"language\"\r\n\r\n"+want+"\r\n") {
			t.Errorf("chose %q: %s", chosen, api.body)
		}
	}
}

// Text assets are compressed for browsers that take it, once.
func TestAssetCompression(t *testing.T) {
	u := New(Deps{NoAuth: true})
	get := func(encoding string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/app/assets/app.css", nil)
		if encoding != "" {
			r.Header.Set("Accept-Encoding", encoding)
		}
		u.ServeHTTP(w, r)
		return w
	}
	plain, zipped := get(""), get("br, gzip")
	if plain.Header().Get("Content-Encoding") != "" || zipped.Header().Get("Content-Encoding") != "gzip" || zipped.Body.Len() >= plain.Body.Len()/2 ||
		zipped.Header().Get("Vary") != "Accept-Encoding" || plain.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("%d then %d bytes: %v", plain.Body.Len(), zipped.Body.Len(), zipped.Header())
	}
	back, err := gzip.NewReader(zipped.Body)
	if err != nil {
		t.Fatal(err)
	}
	unzipped, _ := io.ReadAll(back)
	if !bytes.Equal(unzipped, plain.Body.Bytes()) {
		t.Error("the compressed file is not the file")
	}
}
