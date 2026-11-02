package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The Video studio (DESIGN §22.18): text to video, or a picture to animate for
// image-to-video models, at each model's fixed profile. A job runs on the API
// (/v1/videos) as the signed-in person. The API has no job list, so the page
// keeps a note per job and rebuilds the tray from them; a job the API has
// forgotten leaves the tray.

const videoFormLimit = 9 << 20 // the API's multipart limit

// videoLabModel is a selected video model the Lab can run, with its profile.
type videoLabModel struct {
	labModel
	Width, Height, Frames, FPS int
	Image                      bool // image-to-video: accepts a starting picture
	Deadline                   time.Duration
}

// Length, Shape and Limit say the fixed profile in words.
func (m videoLabModel) Length() string {
	if m.FPS <= 0 {
		return ""
	}
	seconds := int(math.Round(float64(m.Frames) / float64(m.FPS)))
	if seconds == 1 {
		return "About 1 second"
	}
	return fmt.Sprintf("About %d seconds", seconds)
}

func (m videoLabModel) Shape() string {
	switch {
	case m.Width > m.Height:
		return "wide"
	case m.Height > m.Width:
		return "tall"
	}
	return "square"
}

func (m videoLabModel) Limit() string {
	minutes := int(m.Deadline.Minutes())
	if minutes <= 0 {
		return "a few minutes"
	}
	if minutes == 1 {
		return "a minute"
	}
	return fmt.Sprintf("%d minutes", minutes)
}

// videoRequest is what a job was asked for, to show and to try again.
type videoRequest struct {
	Model, Negative string
	Picture         string // a result id: the prepared starting picture
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
			FPS: q.FPS, Image: s.VideoRuntime.Mode == "ti2v", Deadline: time.Duration(q.JobDeadlineMs) * time.Millisecond})
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

// recentPictures are the person's latest generated pictures, to start a video from.
func (u *UI) recentPictures(r *http.Request) []labPicture {
	var out []labPicture
	for _, it := range u.labRuns(r) {
		for _, p := range it.Pictures {
			if len(out) < 6 {
				out = append(out, p)
			}
		}
	}
	return out
}

// labVideoOptions refreshes the profile and picture input on a model change.
func (u *UI) labVideoOptions(w http.ResponseWriter, r *http.Request) {
	u.render(w, r, "lab", map[string]any{"VideoModel": u.videoModel(r.URL.Query().Get("model")), "RecentPictures": u.recentPictures(r)})
}

// labVideo starts a job.
func (u *UI) labVideo(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(videoFormLimit); err != nil {
		u.labOutcome(w, r, map[string]any{"Error": "The picture is too large or could not be read."})
		return
	}
	q := videoRequest{Model: r.FormValue("model"), Negative: strings.TrimSpace(r.FormValue("negative_prompt"))}
	var picture []byte
	if f, _, err := r.FormFile("input_reference"); err == nil {
		picture, _ = io.ReadAll(f)
		f.Close()
	}
	if len(picture) > 0 {
		q.Picture = u.results.put(u.caller(r).OwnerID(), "image/png", picture)
	}
	u.startVideo(w, r, r.FormValue("prompt"), q, picture)
}

// startVideo creates the job and answers with its tray item.
func (u *UI) startVideo(w http.ResponseWriter, r *http.Request, prompt string, q videoRequest, picture []byte) {
	m := u.videoModel(q.Model)
	if m == nil {
		u.labOutcome(w, r, map[string]any{"Error": "Choose a video model."})
		return
	}
	req := map[string]any{"model": m.ID, "prompt": prompt, "width": m.Width, "height": m.Height,
		"frames": m.Frames, "fps": m.FPS, "input": map[string]any{"kind": "text"}}
	if q.Negative != "" {
		req["negative_prompt"] = q.Negative
	}
	if len(picture) > 0 {
		req["input_reference"] = base64.StdEncoding.EncodeToString(picture)
	}
	body, _ := json.Marshal(req)
	res := u.call(r, "POST", "/v1/videos", "application/json", body)
	if msg := res.apiError(); msg != "" {
		// One job runs at a time; the words stay in the prompt for the next try.
		if bytes.Contains(res.Body, []byte("video_job_busy")) {
			msg = "One video at a time. Stop the one that’s running, or send this when it finishes."
		}
		u.labOutcome(w, r, map[string]any{"Error": msg})
		return
	}
	var job videoJobView
	if json.Unmarshal(res.Body, &job) != nil || job.ID == "" {
		u.labOutcome(w, r, map[string]any{"Error": "The video service returned an unexpected response."})
		return
	}
	owner := u.caller(r).OwnerID()
	u.notes.put(owner, job.ID, prompt)
	u.notes.setVideo(owner, job.ID, q)
	w.Header().Set("HX-Retarget", "#lab-items")
	w.Header().Set("HX-Reswap", "afterbegin")
	u.fragment(w, r, "lab", "lab-item", u.videoItem(job, u.notes.get(owner, job.ID)))
}

// labVideoAgain makes a job's video again: same words, model and picture.
func (u *UI) labVideoAgain(w http.ResponseWriter, r *http.Request) {
	owner := u.caller(r).OwnerID()
	note := u.notes.get(owner, r.PathValue("id"))
	if note == nil || note.video == nil {
		u.labOutcome(w, r, map[string]any{"Error": "This video is gone. Describe it again."})
		return
	}
	var picture []byte
	if note.video.Picture != "" {
		kept := u.results.get(owner, note.video.Picture)
		if kept == nil {
			u.labOutcome(w, r, map[string]any{"Error": "The starting picture has expired. Choose it again."})
			return
		}
		picture = kept.body
	}
	u.startVideo(w, r, note.prompt, *note.video, picture)
}

// labVideoStatus is the tray item while a job runs.
func (u *UI) labVideoStatus(w http.ResponseWriter, r *http.Request) {
	u.videoRender(w, r, u.call(r, "GET", "/v1/videos/"+url.PathEscape(r.PathValue("id")), "", nil))
}

func (u *UI) labVideoCancel(w http.ResponseWriter, r *http.Request) {
	u.videoRender(w, r, u.call(r, "POST", "/v1/videos/"+url.PathEscape(r.PathValue("id"))+"/cancel", "", nil))
}

// labVideoRemove deletes a job and its file. One still running stops first.
func (u *UI) labVideoRemove(w http.ResponseWriter, r *http.Request) {
	id := url.PathEscape(r.PathValue("id"))
	u.call(r, "DELETE", "/v1/videos/"+id, "", nil)
	u.notes.drop(u.caller(r).OwnerID(), r.PathValue("id"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
}

// labActive says how many of the person's video jobs and scripts are running,
// for the indicator on other pages. They are not detached runs.
func (u *UI) labActive(w http.ResponseWriter, r *http.Request) {
	counts := map[string]int{"video": 0, "speech": 0}
	var items []labItemView
	if u.caller(r).Can("inference:video") {
		items = u.videoItems(r)
	}
	for _, it := range append(items, u.speechItems(r)...) {
		if it.Active {
			counts[it.Tool]++
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(counts)
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

// videoJobView is the part of a job the tray uses.
type videoJobView struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	CreatedAt int64  `json:"created_at"`
	Error     *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Reason string `json:"cancellation_reason"`
}

// videoRender answers with a job's tray item; a job the API has forgotten
// leaves the tray.
func (u *UI) videoRender(w http.ResponseWriter, r *http.Request, res APIResult) {
	owner := u.caller(r).OwnerID()
	var job videoJobView
	if res.Status == http.StatusNotFound || json.Unmarshal(res.Body, &job) != nil || job.ID == "" {
		u.notes.drop(owner, r.PathValue("id"))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		return
	}
	u.fragment(w, r, "lab", "lab-item", u.videoItem(job, u.notes.get(owner, job.ID)))
}

// videoItems are the person's video jobs, newest first.
func (u *UI) videoItems(r *http.Request) []labItemView {
	owner := u.caller(r).OwnerID()
	var out []labItemView
	for _, id := range u.notes.videos(owner) {
		res := u.call(r, "GET", "/v1/videos/"+url.PathEscape(id), "", nil)
		var job videoJobView
		if res.Status == http.StatusNotFound || json.Unmarshal(res.Body, &job) != nil || job.ID == "" {
			u.notes.drop(owner, id)
			continue
		}
		out = append(out, u.videoItem(job, u.notes.get(owner, id)))
	}
	return out
}

// videoFailure says why a job failed, in words and with what to do next.
func videoFailure(code, message string) string {
	switch code {
	case "insufficient_memory":
		return "There isn’t enough free memory to make this. Unload other models or close apps, then try again."
	case "model_install_consent_required":
		return "This model isn’t fully installed. Finish installing it, then try again."
	}
	if message != "" {
		return message
	}
	return "This video didn’t finish. Try again."
}

// videoItem describes a job for the tray.
func (u *UI) videoItem(job videoJobView, note *labNote) labItemView {
	at := time.Unix(job.CreatedAt, 0)
	v := labItemView{ID: job.ID, Tool: "video", Status: job.Status, Active: job.Status == "queued" || job.Status == "in_progress",
		At: at, Ago: agoText(at, time.Now()), Shape: "square"}
	if note != nil {
		v.Caption, v.Prompt = note.caption, note.prompt
		if q := note.video; q != nil {
			v.Again = true
			if q.Picture != "" {
				v.Thumb = "/app/lab/results/" + q.Picture
			}
			if m := u.videoModel(q.Model); m != nil {
				v.Shape = m.Shape()
				v.Slow = v.Active && m.Deadline > 0 && time.Since(at) > m.Deadline/2
			}
		}
	}
	switch job.Status {
	case "queued":
		v.Message = "Waiting for its turn…"
	case "in_progress":
		v.Message = "Making the video…"
	case "completed":
		v.Video = "/app/lab/video/" + job.ID + "/content"
	case "failed":
		code, message := "", ""
		if job.Error != nil {
			code, message = job.Error.Code, job.Error.Message
		}
		v.Message = videoFailure(code, message)
		v.NeedsMemory = code == "insufficient_memory"
	case "cancelled":
		v.Message = map[string]string{"cancelled": "Stopped. Nothing was saved.", "deadline": "Stopped: this took longer than this model allows.",
			"shutdown": "Stopped: LocalBase was shutting down. Try again.", "backend": "The video runtime stopped this. Try again."}[job.Reason]
		if v.Message == "" {
			v.Message = "Stopped. Nothing was saved."
		}
	}
	v.Announce = v.Message
	if job.Status == "completed" {
		v.Announce = "The video is ready."
	}
	return v
}
