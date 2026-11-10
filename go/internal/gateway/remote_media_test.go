package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
)

// A chat content part carries its content. The gateway refuses one that names
// an address instead, with one code, before anything reaches a backend.

const (
	remoteMediaOrigin = "https://lb.example.test"
	remoteMediaHost   = "lb.example.test"
	remoteImageText   = "This gateway does not fetch images from web addresses; send the image itself as a data: URL."
	remoteAudioText   = "This gateway does not fetch audio from web addresses; send the audio itself as base64."
	remoteFileText    = "This gateway does not fetch files from web addresses; send the file itself as a data: URL."
	onePixelPNG       = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
)

// mediaRoute is one way to reach chat: an API key, or a signed-in browser
// through the /app/api forward. Each holds only inference:chat.
type mediaRoute struct {
	name, path string
	headers    func(*http.Request)
}

func newMediaGateway(t *testing.T) (*mediaGatewayState, []mediaRoute) {
	t.Helper()
	const issuer = "https://accounts.example.test"
	ts, llm, _ := newConfiguredGateway(t, host{}, func(c *Config) {
		c.NoAuth = false
		c.Browser = auth.NewBrowser(c.Store, c.Root.Dir, &auth.BrowserAccess{Origin: remoteMediaOrigin, Permissions: []string{"inference:chat"},
			Provider: auth.Provider{Kind: "direct", Registrations: []auth.Registration{{Kind: "oidc", ID: "google", Name: "Google", Issuer: issuer,
				ClientID: "cid", ClientAuthentication: &auth.ClientAuth{Kind: "none"}}}}}, c.Log)
	})
	srv := ts.Config.Handler.(*Server)
	ctx := context.Background()
	_, key, err := srv.cfg.Store.CreateAPIKey(ctx, "chat", []string{"inference:chat"})
	if err != nil {
		t.Fatal(err)
	}
	token, err := auth.CreateSession(ctx, srv.cfg.Store, "browser:person", auth.Identity{Issuer: issuer, Subject: "person", VerifiedEmail: "person@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	return &mediaGatewayState{base: ts.URL, llm: llm}, []mediaRoute{
		{"api key", "/v1/chat/completions", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+key) }},
		{"signed-in page", "/app/api/v1/chat/completions", func(r *http.Request) {
			r.Host = remoteMediaHost
			r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: token})
			r.Header.Set("Origin", remoteMediaOrigin)
			r.Header.Set("X-Localbase-Ui", "1")
			r.Header.Set("Sec-Fetch-Site", "same-origin")
		}},
	}
}

type mediaGatewayState struct {
	base string
	llm  *runtime.LLM
}

type mediaError struct {
	status               int
	code, param, message string
	raw                  string
}

func (g *mediaGatewayState) send(t *testing.T, route mediaRoute, body string) mediaError {
	t.Helper()
	req, err := http.NewRequest("POST", g.base+route.path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	route.headers(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var parsed struct {
		Error struct {
			Code    any
			Param   any
			Message string
		}
	}
	json.Unmarshal(b, &parsed)
	code, _ := parsed.Error.Code.(string)
	param, _ := parsed.Error.Param.(string)
	return mediaError{status: resp.StatusCode, code: code, param: param, message: parsed.Error.Message, raw: string(b)}
}

func (g *mediaGatewayState) noBackend(t *testing.T) {
	t.Helper()
	if id, _, _ := g.llm.Running(); id != "" {
		t.Errorf("a backend was started for %q, so something could have reached it", id)
	}
}

func jsonString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func userParts(parts ...string) string {
	return `{"model":"` + gatewaytest.ChatModel + `","messages":[{"role":"user","content":[` + strings.Join(parts, ",") + `]}]}`
}

const textPartJSON = `{"type":"text","text":"look"}`

func imagePart(t *testing.T, url string) string {
	return `{"type":"image_url","image_url":{"url":` + jsonString(t, url) + `}}`
}

func TestContentFromAnAddressIsRefusedBeforeABackend(t *testing.T) {
	g, routes := newMediaGateway(t)
	type refusal struct{ name, part, param, message string }
	var cases []refusal
	const imageParam = "messages.0.content.1.image_url.url"
	for _, url := range []string{
		"https://example.test/a.png",
		"http://example.test/a.png",
		"HTTP://EXAMPLE.TEST/A.PNG",
		"http://127.0.0.1:9/a.png",
		"http://[::1]:9/a.png",
		"file:///tmp/a.png",
		"/tmp/a.png",
		"ftp://example.test/a.png",
		"//example.test/a.png",
		"example.test/a.png",
		"blob:https://example.test/a",
		" data:image/png;base64,QUJD",
		"dat",
	} {
		cases = append(cases, refusal{"image " + url, imagePart(t, url), imageParam, remoteImageText})
	}
	for _, v := range []string{"https://example.test/a.wav", "http://127.0.0.1:9/a.wav", "http://localhost/a", "file:///tmp/a.wav", "file:/tmp/a", "//example.test/a.wav", "example.test:80/a"} {
		cases = append(cases, refusal{"audio " + v, `{"type":"input_audio","input_audio":{"format":"wav","data":` + jsonString(t, v) + `}}`,
			"messages.0.content.1.input_audio.data", remoteAudioText})
	}
	for _, v := range []string{"https://example.test/a.pdf", "http://127.0.0.1:9/a.pdf", "http://localhost/a", "file:///tmp/a.pdf", "//example.test/a.pdf"} {
		cases = append(cases,
			refusal{"file data " + v, `{"type":"file","file":{"filename":"a.pdf","file_data":` + jsonString(t, v) + `}}`, "messages.0.content.1.file.file_data", remoteFileText},
			refusal{"file id " + v, `{"type":"file","file":{"file_id":` + jsonString(t, v) + `}}`, "messages.0.content.1.file.file_id", remoteFileText})
	}
	for _, route := range routes {
		for _, c := range cases {
			t.Run(route.name+"/"+c.name, func(t *testing.T) {
				got := g.send(t, route, userParts(textPartJSON, c.part))
				if got.status != 400 || got.code != "remote_media_not_allowed" || got.param != c.param || got.message != c.message {
					t.Errorf("got %d %q %q: %s", got.status, got.code, got.param, got.raw)
				}
			})
		}
	}
	g.noBackend(t)
}

func TestTheRefusalNamesTheFirstAddressAndWinsOverOtherProblems(t *testing.T) {
	g, routes := newMediaGateway(t)
	url := imagePart(t, "https://example.test/a.png")
	cases := []struct{ name, body, param string }{
		{"a later message", `{"model":"` + gatewaytest.ChatModel + `","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yes"},{"role":"user","content":[` + url + `]}]}`,
			"messages.2.content.0.image_url.url"},
		{"after a good image", userParts(imagePart(t, onePixelPNG), url), "messages.0.content.1.image_url.url"},
		{"two addresses", userParts(url, imagePart(t, "https://example.test/b.png")), "messages.0.content.0.image_url.url"},
		{"another field is wrong", `{"model":"` + gatewaytest.ChatModel + `","temperature":9,"messages":[{"role":"user","content":[` + url + `]}]}`, "messages.0.content.0.image_url.url"},
		{"no model", `{"messages":[{"role":"user","content":[` + url + `]}]}`, "messages.0.content.0.image_url.url"},
		{"a model this gateway lacks", `{"model":"no-such-model","messages":[{"role":"user","content":[` + url + `]}]}`, "messages.0.content.0.image_url.url"},
		{"streaming", `{"model":"` + gatewaytest.ChatModel + `","stream":true,"messages":[{"role":"user","content":[` + url + `]}]}`, "messages.0.content.0.image_url.url"},
		{"a bad detail", userParts(`{"type":"image_url","image_url":{"detail":"x","url":"https://example.test/a.png"}}`), "messages.0.content.0.image_url.url"},
		{"audio in a bad format", userParts(`{"type":"input_audio","input_audio":{"format":"ogg","data":"https://example.test/a"}}`), "messages.0.content.0.input_audio.data"},
		{"a file with an extra field", userParts(`{"type":"file","file":{"filename":"a","file_id":"b","file_data":"https://example.test/a"}}`), "messages.0.content.0.file.file_data"},
		{"with a detail", userParts(`{"type":"image_url","image_url":{"detail":"low","url":"https://example.test/a.png"}}`), "messages.0.content.0.image_url.url"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := g.send(t, routes[0], c.body)
			if got.status != 400 || got.code != "remote_media_not_allowed" || got.param != c.param {
				t.Errorf("got %d %q %q: %s", got.status, got.code, got.param, got.raw)
			}
		})
	}
	g.noBackend(t)
}

// Parts the gateway has never accepted stay refused as they were, whatever
// they name, and other roles still take only text.
func TestPartsThatWereNeverAcceptedStayRefused(t *testing.T) {
	g, routes := newMediaGateway(t)
	const u = `"https://example.test/a"`
	userPartsRefused := []string{
		`{"type":"video_url","video_url":{"url":` + u + `}}`,
		`{"type":"audio_url","audio_url":{"url":` + u + `}}`,
		`{"type":"input_image","image_url":` + u + `}`,
		`{"type":"input_file","file_url":` + u + `}`,
		`{"type":"image_url","image_url":` + u + `}`,
		`{"type":"image_url","image_url":{}}`,
		`{"type":"IMAGE_URL","image_url":{"url":` + u + `}}`,
	}
	bodies := map[string]string{}
	for _, p := range userPartsRefused {
		bodies["user "+p] = userParts(p)
	}
	for _, role := range []string{"system", "developer", "assistant"} {
		bodies[role+" image"] = `{"model":"` + gatewaytest.ChatModel + `","messages":[{"role":"` + role + `","content":[{"type":"image_url","image_url":{"url":` + u + `}}]}]}`
	}
	bodies["tool image"] = `{"model":"` + gatewaytest.ChatModel + `","messages":[{"role":"tool","tool_call_id":"c","content":[{"type":"image_url","image_url":{"url":` + u + `}}]}]}`
	for _, route := range routes {
		for name, body := range bodies {
			t.Run(route.name+"/"+name, func(t *testing.T) {
				if got := g.send(t, route, body); got.status != 400 || got.code != "validation_failed" {
					t.Errorf("got %d %q: %s", got.status, got.code, got.raw)
				}
			})
		}
	}
	g.noBackend(t)
}

func TestContentSentDirectlyStillWorks(t *testing.T) {
	g, routes := newMediaGateway(t)
	cases := map[string]string{
		"text":              `{"model":"` + gatewaytest.ChatModel + `","messages":[{"role":"user","content":"hello"}]}`,
		"text parts":        userParts(textPartJSON),
		"an image":          userParts(textPartJSON, imagePart(t, onePixelPNG)),
		"an image only":     userParts(imagePart(t, onePixelPNG)),
		"an image, detail":  userParts(`{"type":"image_url","image_url":{"detail":"high","url":` + jsonString(t, onePixelPNG) + `}}`),
		"a scheme in caps":  userParts(imagePart(t, "DATA:image/png;base64,QUJD")),
		"audio as base64":   userParts(`{"type":"input_audio","input_audio":{"format":"mp3","data":"//uQxAAAAAAAAAAAAAAA+/=="}}`),
		"audio, urlsafe":    userParts(`{"type":"input_audio","input_audio":{"format":"wav","data":"UklG-Rg_"}}`),
		"audio, wrapped":    userParts(`{"type":"input_audio","input_audio":{"format":"wav","data":"UklG\r\nRg=="}}`),
		"audio, spaces":     userParts(`{"type":"input_audio","input_audio":{"format":"wav","data":"UklG Rg== "}}`),
		"audio, tabs":       userParts(`{"type":"input_audio","input_audio":{"format":"wav","data":"UklG\tRg=="}}`),
		"audio, plus":       userParts(`{"type":"input_audio","input_audio":{"format":"wav","data":"Ukl+"}}`),
		"audio as data":     userParts(`{"type":"input_audio","input_audio":{"format":"wav","data":"data:audio/wav;base64,UklG"}}`),
		"a file as data":    userParts(`{"type":"file","file":{"filename":"a.pdf","file_data":"data:application/pdf;base64,QUJD"}}`),
		"a file as base64":  userParts(`{"type":"file","file":{"filename":"a.pdf","file_data":"QUJD"}}`),
		"a file by id":      userParts(`{"type":"file","file":{"file_id":"file-abc_123"}}`),
		"a name with a dot": userParts(`{"type":"file","file":{"filename":"https://example.test/a.pdf","file_data":"QUJD"}}`),
	}
	for _, route := range routes {
		for name, body := range cases {
			t.Run(route.name+"/"+name, func(t *testing.T) {
				if got := g.send(t, route, body); got.status != 200 {
					t.Errorf("got %d %q: %s", got.status, got.code, got.raw)
				} else if !strings.Contains(got.raw, "hello from") {
					t.Errorf("the backend did not answer: %s", got.raw)
				}
			})
		}
	}
	if id, _, _ := g.llm.Running(); id != gatewaytest.ChatModel {
		t.Errorf("the backend should be running %q, got %q", gatewaytest.ChatModel, id)
	}
}

// A detached run takes the same route, so its request gets the same refusal
// and no run is left behind.
func TestADetachedRunIsRefusedTheSameWay(t *testing.T) {
	ts, llm, _ := newTestGateway(t, host{})
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	code, m := c.Do("POST", "/_localbase/runs/v1/chat/completions", userParts(imagePart(t, "https://example.test/a.png")))
	e, _ := m["error"].(map[string]any)
	if code != 400 || e["code"] != "remote_media_not_allowed" {
		t.Errorf("create: %d %v", code, m)
	}
	if _, list := c.Do("GET", "/_localbase/runs", ""); len(list["data"].([]any)) != 0 {
		t.Errorf("a run was left behind: %v", list)
	}
	if id, _, _ := llm.Running(); id != "" {
		t.Errorf("a backend was started for %q", id)
	}
}
