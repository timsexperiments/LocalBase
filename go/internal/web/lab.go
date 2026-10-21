package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

// The Lab tries the selected media models from the browser: generate an
// image, speak text, transcribe a recording. Each form posts to a UI handler
// that calls the API route in-process as the signed-in person (Deps.Call),
// so the page gets exactly the API's validation and errors.

// APIResult is an in-process API response.
type APIResult struct {
	Status int
	Header http.Header
	Body   []byte
}

// apiError is the message of an API error envelope ("" if none).
func (a APIResult) apiError() string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(a.Body, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	if a.Status >= 400 {
		return http.StatusText(a.Status)
	}
	return ""
}

// results keeps generated media briefly so the page can play or show it
// from 'self' (the CSP allows no data: audio). Bounded by count and age, and
// bound to the person who made it.
type labResult struct {
	owner, ctype string
	body         []byte
	at           time.Time
}

type resultStore struct {
	mu    sync.Mutex
	items map[string]*labResult
	order []string
}

const (
	resultLimit = 16
	resultTTL   = 30 * time.Minute
)

func (s *resultStore) put(owner, ctype string, body []byte) string {
	b := make([]byte, 16)
	rand.Read(b)
	id := hex.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		s.items = map[string]*labResult{}
	}
	s.items[id] = &labResult{owner: owner, ctype: ctype, body: body, at: time.Now()}
	s.order = append(s.order, id)
	for len(s.order) > resultLimit {
		delete(s.items, s.order[0])
		s.order = s.order[1:]
	}
	return id
}

func (s *resultStore) get(owner, id string) *labResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.items[id]
	if r == nil || r.owner != owner || time.Since(r.at) > resultTTL {
		return nil
	}
	return r
}

func (u *UI) caller(r *http.Request) *auth.Principal {
	p, _ := r.Context().Value(ctxKey{}).(*auth.Principal)
	return p
}

func (u *UI) call(r *http.Request, method, path, ctype string, body []byte) APIResult {
	if u.d.Call == nil {
		return APIResult{Status: http.StatusServiceUnavailable}
	}
	return u.d.Call(r.Context(), u.caller(r), method, path, ctype, body)
}

// uploading is mutating for multipart forms: the same origin checks, a
// larger body limit, and no form parsing (the API parses the body).
func (u *UI) uploading(permission string, limit int64, next http.HandlerFunc) http.HandlerFunc {
	return u.need(permission, func(w http.ResponseWriter, r *http.Request) {
		if !sameOriginUI(r) || !u.csrfValid(r) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next(w, r)
	})
}

type labModel struct {
	ID, Name string
	Active   bool
}

// labModels are the selected models of a kind, the active one first.
func (u *UI) labModels(kind string) []labModel {
	if u.d.LLM == nil {
		return nil
	}
	cfg := u.d.LLM.Config()
	var out []labModel
	for _, id := range cfg.Selected[kind] {
		m := labModel{ID: id, Name: id, Active: id == cfg.Active[kind]}
		if s := u.d.Catalog.ByID(id); s != nil {
			m.Name = s.Name()
		}
		out = append(out, m)
	}
	slices.SortStableFunc(out, func(a, b labModel) int {
		switch {
		case a.Active && !b.Active:
			return -1
		case b.Active && !a.Active:
			return 1
		}
		return 0
	})
	return out
}

type labVoice struct{ ID, Name string }

// labVoices are the voices a speech model accepts: its own, then custom
// voices for models that clone from a reference.
func (u *UI) labVoices(ctx context.Context, modelID string) []labVoice {
	s := u.d.Catalog.ByID(modelID)
	if s == nil || s.TTSRuntime == nil {
		return nil
	}
	rt := s.TTSRuntime
	var out []labVoice
	if rt.Kind == "llama-tts" {
		out = append(out, labVoice{"default", "Default"})
		for _, v := range rt.ReferenceVoices {
			out = append(out, labVoice{v.Name, voiceLabel(v.Name)})
		}
		if u.d.Voices != nil {
			if list, err := u.d.Voices(ctx); err == nil {
				for _, v := range list {
					out = append(out, labVoice{v.ID, v.Name})
				}
			}
		}
		return out
	}
	for _, v := range rt.Voices {
		out = append(out, labVoice{v.Name, voiceLabel(v.Name)})
	}
	return out
}

func voiceLabel(name string) string {
	if i := strings.IndexByte(name, '_'); i >= 0 {
		name = name[i+1:]
	}
	if name == "" {
		return name
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

func (u *UI) lab(w http.ResponseWriter, r *http.Request) {
	tab := r.URL.Query().Get("tab")
	if !slices.Contains([]string{"image", "video", "speech", "transcribe"}, tab) {
		tab = "image"
	}
	p := u.caller(r)
	data := map[string]any{"Tab": tab, "Sizes": []string{"512x512", "768x512", "512x768", "256x256", "1024x1024"},
		"CanImage": p.Can("inference:image"), "CanSpeech": p.Can("inference:speech"),
		"CanTranscribe": p.Can("inference:transcription"), "CanManage": p.Can("models:manage"), "CanVideo": p.Can("inference:video")}
	switch tab {
	case "image":
		data["Models"] = u.labModels("image")
	case "video":
		models := u.videoModels()
		data["Models"] = models
		if len(models) > 0 {
			data["VideoModel"] = &models[0]
		}
	case "speech":
		models := u.labModels("tts")
		data["Models"] = models
		if len(models) > 0 {
			data["Voices"] = u.labVoices(r.Context(), models[0].ID)
		}
	case "transcribe":
		data["Models"] = u.labModels("stt")
	}
	u.render(w, r, "lab", data)
}

// labVoiceOptions refreshes the voice list when the speech model changes.
func (u *UI) labVoiceOptions(w http.ResponseWriter, r *http.Request) {
	u.render(w, r, "lab", map[string]any{"Voices": u.labVoices(r.Context(), r.URL.Query().Get("model"))})
}

func (u *UI) labOutcome(w http.ResponseWriter, r *http.Request, data map[string]any) {
	r.Header.Set("HX-Target", "lab-result")
	u.render(w, r, "lab", data)
}

func (u *UI) labImage(w http.ResponseWriter, r *http.Request) {
	req := map[string]any{"prompt": r.PostFormValue("prompt"), "size": r.PostFormValue("size"), "response_format": "b64_json"}
	if m := r.PostFormValue("model"); m != "" {
		req["model"] = m
	}
	if v := strings.TrimSpace(r.PostFormValue("negative_prompt")); v != "" {
		req["negative_prompt"] = v
	}
	for _, k := range []string{"seed", "steps"} {
		if v := strings.TrimSpace(r.PostFormValue(k)); v != "" {
			var n json.Number = json.Number(v)
			if _, err := n.Int64(); err != nil {
				u.labOutcome(w, r, map[string]any{"Error": k + " must be a whole number."})
				return
			}
			req[k] = n
		}
	}
	body, _ := json.Marshal(req)
	res := u.call(r, "POST", "/v1/images/generations", "application/json", body)
	if msg := res.apiError(); msg != "" {
		u.labOutcome(w, r, map[string]any{"Error": msg})
		return
	}
	var out struct {
		Data []struct {
			B64 string `json:"b64_json"`
		} `json:"data"`
	}
	if json.Unmarshal(res.Body, &out) != nil || len(out.Data) == 0 {
		u.labOutcome(w, r, map[string]any{"Error": "The image service returned no image."})
		return
	}
	img, err := base64.StdEncoding.DecodeString(out.Data[0].B64)
	if err != nil {
		u.labOutcome(w, r, map[string]any{"Error": "The image service returned no image."})
		return
	}
	id := u.results.put(u.caller(r).OwnerID(), "image/png", img)
	u.labOutcome(w, r, map[string]any{"Image": "/app/lab/results/" + id})
}

func (u *UI) labSpeech(w http.ResponseWriter, r *http.Request) {
	req := map[string]any{"model": r.PostFormValue("model"), "input": r.PostFormValue("input"),
		"voice": r.PostFormValue("voice"), "response_format": "wav"}
	body, _ := json.Marshal(req)
	res := u.call(r, "POST", "/v1/audio/speech", "application/json", body)
	if msg := res.apiError(); msg != "" {
		u.labOutcome(w, r, map[string]any{"Error": msg})
		return
	}
	id := u.results.put(u.caller(r).OwnerID(), "audio/wav", res.Body)
	u.labOutcome(w, r, map[string]any{"Audio": "/app/lab/results/" + id})
}

const transcribeLimit = 25 << 20

func (u *UI) labTranscribe(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(transcribeLimit); err != nil {
		u.labOutcome(w, r, map[string]any{"Error": "The recording is too large or could not be read."})
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		u.labOutcome(w, r, map[string]any{"Error": "Choose or record audio first."})
		return
	}
	audio, _ := io.ReadAll(f)
	f.Close()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	fw, _ := mw.CreateFormFile("file", hdr.Filename)
	fw.Write(audio)
	for _, k := range []string{"model", "language"} {
		if v := strings.TrimSpace(r.FormValue(k)); v != "" {
			mw.WriteField(k, v)
		}
	}
	mw.WriteField("response_format", "json")
	mw.Close()
	path := "/v1/audio/transcriptions"
	if r.FormValue("translate") == "on" {
		path = "/v1/audio/translations"
	}
	res := u.call(r, "POST", path, mw.FormDataContentType(), b.Bytes())
	if msg := res.apiError(); msg != "" {
		u.labOutcome(w, r, map[string]any{"Error": msg})
		return
	}
	var out struct {
		Text string `json:"text"`
	}
	json.Unmarshal(res.Body, &out)
	u.labOutcome(w, r, map[string]any{"Transcript": out.Text})
}

func (u *UI) labResult(w http.ResponseWriter, r *http.Request) {
	res := u.results.get(u.caller(r).OwnerID(), r.PathValue("id"))
	if res == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", res.ctype)
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, "", res.at, bytes.NewReader(res.body))
}
