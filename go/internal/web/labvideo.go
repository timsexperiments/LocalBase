package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The Lab's video section (TS studio, video mode): text to video, or a
// picture to animate for image-to-video models, at each model's fixed
// profile. The job runs on the API (/v1/videos) as the signed-in person;
// the page polls it, can cancel it, and plays the result.

const videoFormLimit = 9 << 20 // the API's multipart limit

// videoLabModel is a selected video model the Lab can run, with its profile.
type videoLabModel struct {
	labModel
	Width, Height, Frames, FPS int
	Image                      bool // image-to-video: accepts a starting picture
}

// videoModels are the selected text- or image-to-video models (speech to
// video needs portrait and audio inputs the Lab does not collect).
func (u *UI) videoModels() []videoLabModel {
	var out []videoLabModel
	for _, m := range u.labModels("video") {
		s := u.d.Catalog.ByID(m.ID)
		if s == nil || s.VideoRuntime == nil || s.VideoRuntime.Mode == "s2v" {
			continue
		}
		q := s.VideoRuntime.Qualification
		out = append(out, videoLabModel{labModel: m, Width: q.MaxWidth, Height: q.MaxHeight, Frames: q.MaxFrames,
			FPS: q.FPS, Image: s.VideoRuntime.Mode == "ti2v"})
	}
	return out
}

func (u *UI) videoModel(id string) *videoLabModel {
	for _, m := range u.videoModels() {
		if m.ID == id {
			return &m
		}
	}
	return nil
}

// labVideoOptions refreshes the profile and picture input on a model change.
func (u *UI) labVideoOptions(w http.ResponseWriter, r *http.Request) {
	u.render(w, r, "lab", map[string]any{"VideoModel": u.videoModel(r.URL.Query().Get("model"))})
}

// labVideo starts a job.
func (u *UI) labVideo(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(videoFormLimit); err != nil {
		u.labOutcome(w, r, map[string]any{"Error": "The picture is too large or could not be read."})
		return
	}
	m := u.videoModel(r.FormValue("model"))
	if m == nil {
		u.labOutcome(w, r, map[string]any{"Error": "Choose a video model."})
		return
	}
	req := map[string]any{"model": m.ID, "prompt": r.FormValue("prompt"), "width": m.Width, "height": m.Height,
		"frames": m.Frames, "fps": m.FPS, "input": map[string]any{"kind": "text"}}
	if v := strings.TrimSpace(r.FormValue("negative_prompt")); v != "" {
		req["negative_prompt"] = v
	}
	if f, _, err := r.FormFile("input_reference"); err == nil {
		png, _ := io.ReadAll(f)
		f.Close()
		if len(png) > 0 {
			req["input_reference"] = base64.StdEncoding.EncodeToString(png)
		}
	}
	body, _ := json.Marshal(req)
	res := u.call(r, "POST", "/v1/videos", "application/json", body)
	if msg := res.apiError(); msg != "" {
		u.labOutcome(w, r, map[string]any{"Error": msg})
		return
	}
	u.videoJob(w, r, res.Body)
}

// labVideoStatus is the job fragment the page polls.
func (u *UI) labVideoStatus(w http.ResponseWriter, r *http.Request) {
	res := u.call(r, "GET", "/v1/videos/"+url.PathEscape(r.PathValue("id")), "", nil)
	if msg := res.apiError(); msg != "" {
		u.labOutcome(w, r, map[string]any{"Error": msg})
		return
	}
	u.videoJob(w, r, res.Body)
}

func (u *UI) labVideoCancel(w http.ResponseWriter, r *http.Request) {
	res := u.call(r, "POST", "/v1/videos/"+url.PathEscape(r.PathValue("id"))+"/cancel", "", nil)
	if msg := res.apiError(); msg != "" {
		u.labOutcome(w, r, map[string]any{"Error": msg})
		return
	}
	u.videoJob(w, r, res.Body)
}

// labVideoContent plays the result. Range requests are served, which
// Safari needs to play video.
func (u *UI) labVideoContent(w http.ResponseWriter, r *http.Request) {
	res := u.call(r, "GET", "/v1/videos/"+url.PathEscape(r.PathValue("id"))+"/content", "", nil)
	if res.Status != http.StatusOK {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", res.Header.Get("Content-Type"))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(res.Body))
}

// videoJob renders a job: progress with cancel, the video, or why it ended.
func (u *UI) videoJob(w http.ResponseWriter, r *http.Request, raw []byte) {
	var job struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Reason string `json:"cancellation_reason"`
	}
	if json.Unmarshal(raw, &job) != nil || job.ID == "" {
		u.labOutcome(w, r, map[string]any{"Error": "The video service returned an unexpected response."})
		return
	}
	data := map[string]any{"VideoJob": job.ID, "VideoStatus": job.Status}
	switch job.Status {
	case "completed":
		data["Video"] = "/app/lab/video/" + job.ID + "/content"
	case "failed":
		msg := "Video generation failed."
		if job.Error != nil && job.Error.Message != "" {
			msg = job.Error.Message
		}
		data["Error"] = msg
	case "cancelled":
		data["Notice"] = map[string]string{"cancelled": "Cancelled.", "deadline": "Stopped: the video took longer than this model allows.",
			"shutdown": "Stopped: LocalBase is shutting down.", "backend": "The video runtime cancelled the job."}[job.Reason]
	}
	u.labOutcome(w, r, data)
}
