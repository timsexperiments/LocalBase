package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
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
	for _, path := range []string{"/app/lab/speech", "/app/lab/transcribe", "/app/voices", "/app/voices/voice-0123456789ab/delete"} {
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
	w := httptest.NewRecorder()
	u.ServeHTTP(w, r)
	if len(api.calls) != 2 || api.calls[0] != "POST /_localbase/voices multipart/form-data; boundary=b" ||
		api.calls[1] != "GET /_localbase/voices " || !strings.Contains(w.Body.String(), "Added N.") {
		t.Fatalf("create: %q %s", api.calls, w.Body.String())
	}
}
