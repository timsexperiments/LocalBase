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
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(a.Body, &e) == nil && e.Error.Code == "model_unloading" {
		return unloadingMessage
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
	resultLimit = 48 // batches of up to four
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
	ID, Name    string
	Active      bool
	EnglishOnly bool // a transcription model that hears English only
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

// labVoice is a voice to choose: a person's own, or the model's.
type labVoice struct {
	ID, Name, Preview string
	Mine, Checked     bool
}

// labVoices are the voices a speech model accepts: its own, then custom
// voices for models that clone from a reference.
func (u *UI) labVoices(ctx context.Context, modelID string) []labVoice {
	if u.d.Catalog == nil {
		return nil
	}
	s := u.d.Catalog.ByID(modelID)
	if s == nil || s.TTSRuntime == nil {
		return nil
	}
	rt := s.TTSRuntime
	var out []labVoice
	if rt.Kind == "llama-tts" {
		out = append(out, labVoice{ID: "default", Name: "Default"})
		for _, v := range rt.ReferenceVoices {
			out = append(out, labVoice{ID: v.Name, Name: voiceLabel(v.Name)})
		}
		if u.d.Voices != nil {
			if list, err := u.d.Voices(ctx); err == nil {
				for _, v := range list {
					out = append(out, labVoice{ID: v.ID, Name: v.Name, Mine: true})
				}
			}
		}
		return out
	}
	for _, v := range rt.Voices {
		out = append(out, labVoice{ID: v.Name, Name: voiceLabel(v.Name)})
	}
	return out
}

// voiceChoices lists a person's own voices first and checks one: the one asked
// for, or else the model's first.
func (u *UI) voiceChoices(ctx context.Context, model, checked string) []labVoice {
	list := u.labVoices(ctx, model)
	slices.SortStableFunc(list, func(a, b labVoice) int {
		switch {
		case a.Mine && !b.Mine:
			return -1
		case b.Mine && !a.Mine:
			return 1
		}
		return 0
	})
	picked := false
	for i := range list {
		list[i].Preview = previewURL(model, list[i].ID)
		if list[i].ID == checked {
			list[i].Checked, picked = true, true
		}
	}
	for i := range list {
		if !picked && !list[i].Mine {
			list[i].Checked, picked = true, true
		}
	}
	if !picked && len(list) > 0 {
		list[0].Checked = true
	}
	return list
}

// voiceClones says whether a model can speak in a recorded voice.
func (u *UI) voiceClones(model string) bool {
	if u.d.Catalog == nil {
		return false
	}
	s := u.d.Catalog.ByID(model)
	return s != nil && s.TTSRuntime != nil && s.TTSRuntime.Kind == "llama-tts"
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

// lab draws one tool's page: its results, and its prompt when it can run.
func (u *UI) lab(w http.ResponseWriter, r *http.Request, tab string) {
	p := u.caller(r)
	allowed, hasModel, _ := u.toolAvailable(r, tab)
	data := map[string]any{"Tab": tab, "ToolName": studioNames[tab], "Ready": allowed && hasModel, "Sizes": []string{"512x512", "768x512", "512x768", "256x256", "1024x1024"},
		"CanImage": p.Can("inference:image"), "CanSpeech": p.Can("inference:speech"),
		"CanTranscribe": p.Can("inference:transcription"), "CanManage": p.Can("models:manage"), "CanVideo": p.Can("inference:video"), "CanDecide": p.Can("inference:decision")}
	var items []labItemView
	for _, it := range u.labRuns(r) {
		if it.Tool == tab {
			items = append(items, it)
		}
	}
	data["Items"] = items
	switch tab {
	case "image":
		data["Models"] = u.installedOnly(u.labModels("image"))
		data["Looks"] = imageLooks
	case "video":
		models := u.readyVideoModels()
		data["Models"] = models
		if len(models) > 0 {
			data["VideoModel"] = &models[0]
		}
		data["RecentPictures"] = u.recentPictures(r)
	case "speech":
		models := u.installedOnly(u.labModels("tts"))
		data["Models"] = models
		if len(models) > 0 {
			data["Voices"] = u.voiceChoices(r.Context(), models[0].ID, "")
			data["VoiceClones"] = u.voiceClones(models[0].ID)
			if p.Can("models:manage") {
				data["VoiceAdd"] = u.voiceData(r, nil)["List"]
			}
			data["VoiceModel"] = models[0].ID
		}
	case "transcribe":
		data["Models"] = u.installedOnly(u.sttModels())
		data["Languages"] = transcriptLanguages
	case "decisions":
		data["Models"] = u.installedOnly(u.decisionModels())
	}
	u.render(w, r, "lab", data)
}

// labVoiceOptions refreshes the voice list when the speech model changes.
func (u *UI) labVoiceOptions(w http.ResponseWriter, r *http.Request) {
	model := r.URL.Query().Get("model")
	u.render(w, r, "lab", map[string]any{"Voices": u.voiceChoices(r.Context(), model, ""), "VoiceClones": u.voiceClones(model), "CanManage": u.caller(r).Can("models:manage")})
}

func (u *UI) labOutcome(w http.ResponseWriter, r *http.Request, data map[string]any) {
	r.Header.Set("HX-Target", "lab-result")
	u.render(w, r, "lab", data)
}

func (u *UI) labImage(w http.ResponseWriter, r *http.Request) {
	q, msg := imageForm(r)
	if msg != "" {
		u.labOutcome(w, r, map[string]any{"Error": msg})
		return
	}
	if u.startImages(w, r, q) {
		return
	}
	// Without runs: directly, showing the first picture.
	if q.Seed < 0 {
		q.Seed = randomSeed()
	}
	res := u.call(r, "POST", "/v1/images/generations", "application/json", q.body())
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

const transcribeLimit = 25 << 20

func (u *UI) labTranscribe(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(transcribeLimit); err != nil {
		u.labOutcome(w, r, map[string]any{"Error": "This is too long or could not be read. About 13 minutes is the most that fits at a time."})
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
	if v := strings.TrimSpace(r.FormValue("model")); v != "" {
		mw.WriteField("model", v)
	}
	// Whatever the person chose, else detect: with no language the runtime takes speech
	// as English. It stays safe on English-only models.
	language := strings.TrimSpace(r.FormValue("language"))
	if language == "" {
		language = "auto"
	}
	mw.WriteField("language", language)
	// verbose_json: the times of each stretch, where the model gives them.
	mw.WriteField("response_format", "verbose_json")
	mw.Close()
	path := "/v1/audio/transcriptions"
	translated := r.FormValue("translate") == "on"
	if translated {
		path = "/v1/audio/translations"
	}
	owner := u.caller(r).OwnerID()
	remember := func(id string) {
		u.notes.setTranscript(owner, id, u.results.put(owner, "audio/wav", audio), translated)
	}
	if u.labStart(w, r, path, mw.FormDataContentType(), transcriptCaption(hdr.Filename), b.Bytes(), remember) {
		return
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
