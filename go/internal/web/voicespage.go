package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/timsexperiments/LocalBase/go/internal/voices"
)

// My voices: custom speech voice profiles through /_localbase/voices, called
// in-process as the signed-in person, so the API's permissions, consent rule
// and audio checks apply unchanged.

type voiceList struct {
	Data      []voices.Voice `json:"data"`
	CanManage bool           `json:"canManage"`
	Limits    struct {
		MinSeconds     int `json:"minSeconds"`
		MaxSeconds     int `json:"maxSeconds"`
		MaxUploadBytes int `json:"maxUploadBytes"`
		MaxVoices      int `json:"maxVoices"`
	} `json:"limits"`
	ConsentStatement string `json:"consentStatement"`
}

func (u *UI) voiceData(r *http.Request, extra map[string]any) map[string]any {
	data := map[string]any{}
	res := u.call(r, "GET", "/_localbase/voices", "", nil)
	var list voiceList
	if msg := res.apiError(); msg != "" {
		data["Error"] = msg
	} else if err := json.Unmarshal(res.Body, &list); err != nil {
		data["Error"] = "The voice list could not be read."
	}
	data["List"] = list
	for k, v := range extra {
		data[k] = v
	}
	return data
}

func (u *UI) voicesPage(w http.ResponseWriter, r *http.Request) {
	u.render(w, r, "voices", u.voiceData(r, nil))
}

func (u *UI) voicesFragment(w http.ResponseWriter, r *http.Request, extra map[string]any) {
	r.Header.Set("HX-Target", "voice-list")
	u.render(w, r, "voices", u.voiceData(r, extra))
}

func (u *UI) voiceCreate(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		u.voicesFragment(w, r, map[string]any{"Notice": "The recording is too large.", "Failed": true})
		return
	}
	res := u.call(r, "POST", "/_localbase/voices", r.Header.Get("Content-Type"), body)
	if msg := res.apiError(); msg != "" {
		u.voicesFragment(w, r, map[string]any{"Notice": msg, "Failed": true})
		return
	}
	var v voices.Voice
	json.Unmarshal(res.Body, &v)
	u.voicesFragment(w, r, map[string]any{"Notice": "Added " + v.Name + "."})
}

func (u *UI) voiceRename(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	body, _ := json.Marshal(map[string]string{"name": r.PostFormValue("name")})
	res := u.call(r, "PATCH", "/_localbase/voices/"+url.PathEscape(id), "application/json", body)
	if msg := res.apiError(); msg != "" {
		u.voicesFragment(w, r, map[string]any{"Notice": msg, "Failed": true})
		return
	}
	u.voicesFragment(w, r, map[string]any{"Notice": "Renamed."})
}

func (u *UI) voiceDelete(w http.ResponseWriter, r *http.Request) {
	res := u.call(r, "DELETE", "/_localbase/voices/"+url.PathEscape(r.PathValue("id")), "", nil)
	if msg := res.apiError(); msg != "" {
		u.voicesFragment(w, r, map[string]any{"Notice": msg, "Failed": true})
		return
	}
	u.voicesFragment(w, r, map[string]any{"Notice": "Deleted the voice and its recording."})
}

// voiceAudio replays a stored reference (managers only, as the API rules).
func (u *UI) voiceAudio(w http.ResponseWriter, r *http.Request) {
	res := u.call(r, "GET", "/_localbase/voices/"+url.PathEscape(r.PathValue("id"))+"/audio", "", nil)
	if res.Status != 200 {
		http.Error(w, res.apiError(), res.Status)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Content-Length", strconv.Itoa(len(res.Body)))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Write(res.Body)
}
