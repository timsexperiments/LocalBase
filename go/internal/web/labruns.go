package web

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Lab runs (DESIGN §22.10): when the gateway serves detached runs (§21.9),
// the Lab's image, speech and transcription requests become runs. The page
// shows each as an item in the results tray that polls until the run ends,
// so leaving the page, switching tools or a phone suspending the browser
// loses nothing; the tray is rebuilt from the person's runs on every visit.
// Without runs the Lab calls the API directly, as before.

// labTargets are the media routes a Lab run may target, by tool.
var labTargets = map[string]string{
	"/v1/images/generations":   "image",
	"/v1/audio/transcriptions": "transcribe",
	"/v1/audio/translations":   "transcribe",
}

// labRun is the part of a run object the tray uses.
type labRun struct {
	ID, Target, Status, Reason, Model string
	Error                             *struct{ Code, Message string } `json:"error"`
	CreatedAt                         time.Time
}

// unloadingMessage: a request reached a model while someone unloaded it
// (DESIGN §23); the gateway answers 503 model_unloading.
const unloadingMessage = "The model’s memory was being freed, so this didn’t run. Try again: it loads again when needed."

// labPicture is one image of a result.
type labPicture struct {
	Index, URL string
	Seed       int64
	Removed    bool
}

// picture finds a picture by its index in the batch.
func (n *labNote) picture(index string) *labPicture {
	for i := range n.pictures {
		if n.pictures[i].Index == index {
			return &n.pictures[i]
		}
	}
	return nil
}

// labNote is what the tray remembers about a run, in memory, bound to its
// owner: the prompt as a caption, and the collected result.
type labNote struct {
	owner, caption, prompt, result, text string
	image                                *imageRequest
	video                                *videoRequest
	speech                               *speechJob
	source                               string // a transcript's audio: a result id
	translated                           bool
	language                             string
	segments                             []labSegment
	pictures                             []labPicture
	at                                   time.Time
}

type labNotes struct {
	mu         sync.Mutex
	collecting sync.Mutex
	items      map[string]*labNote
}

func (n *labNotes) get(owner, id string) *labNote {
	n.mu.Lock()
	defer n.mu.Unlock()
	if v := n.items[id]; v != nil && v.owner == owner && time.Since(v.at) < resultTTL+resultTTL {
		return v
	}
	return nil
}

func (n *labNotes) put(owner, id, prompt string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.items == nil {
		n.items = map[string]*labNote{}
	}
	for k, v := range n.items {
		if time.Since(v.at) > resultTTL+resultTTL {
			delete(n.items, k)
		}
	}
	n.items[id] = &labNote{owner: owner, caption: caption(prompt), prompt: prompt, at: time.Now()}
}

func (n *labNotes) setImage(owner, id string, q imageRequest) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if v := n.items[id]; v != nil && v.owner == owner {
		v.image = &q
	}
}

func (n *labNotes) setTranscript(owner, id, source string, translated bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if v := n.items[id]; v != nil && v.owner == owner {
		v.source, v.translated = source, translated
	}
}

func (n *labNotes) setVideo(owner, id string, q videoRequest) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if v := n.items[id]; v != nil && v.owner == owner {
		v.video = &q
	}
}

func (n *labNotes) drop(owner, id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if v := n.items[id]; v != nil && v.owner == owner {
		delete(n.items, id)
	}
}

// videos are the ids of a person's video jobs.
func (n *labNotes) videos(owner string) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for id, v := range n.items {
		if v.owner == owner && v.video != nil && time.Since(v.at) < resultTTL+resultTTL {
			out = append(out, id)
		}
	}
	return out
}

// runsServed: the gateway answers GET /_localbase/runs with a list.
func (u *UI) runsServed(r *http.Request) bool {
	res := u.call(r, "GET", "/_localbase/runs", "", nil)
	var body struct {
		Data []json.RawMessage `json:"data"`
	}
	return res.Status == http.StatusOK && json.Unmarshal(res.Body, &body) == nil && body.Data != nil
}

// labStart starts path as a run and answers with its tray item; false means
// runs are not served and the caller should call path directly.
func (u *UI) labStart(w http.ResponseWriter, r *http.Request, path, ctype, prompt string, body []byte, started ...func(id string)) bool {
	if !u.runsServed(r) {
		return false
	}
	res := u.call(r, "POST", "/_localbase/runs"+path, ctype, body)
	if msg := res.apiError(); msg != "" {
		u.labOutcome(w, r, map[string]any{"Error": msg})
		return true
	}
	var run labRun
	if json.Unmarshal(res.Body, &run) != nil || run.ID == "" {
		u.labOutcome(w, r, map[string]any{"Error": "LocalBase did not start the request. Try again."})
		return true
	}
	u.notes.put(u.caller(r).OwnerID(), run.ID, prompt)
	for _, fn := range started {
		fn(run.ID)
	}
	w.Header().Set("HX-Retarget", "#lab-items")
	w.Header().Set("HX-Reswap", "afterbegin")
	u.fragment(w, r, "lab", "lab-item", u.labItem(r, run))
	return true
}

// labItemView is one tray item.
type labItemView struct {
	ID, Tool, Status, Caption, Message string
	Prompt                             string // to try again (empty for a file)
	Fresh                              bool   // just started: its skeleton may shimmer once
	Size, Shape, Look, LookName, Model string // how a picture was made
	Seed                               int64
	Pictures                           []labPicture // a batch's pictures, without deleted ones
	Tiles                              []int        // placeholders while it is made
	Image, Audio, Transcript           string
	Ago                                string // "2 min ago"
	Active                             bool
	At                                 time.Time
	Video, Thumb, Announce             string // a video job: its file, starting picture, and what to say
	Again, Slow, NeedsMemory           bool
	VoiceName, Length                  string       // a script
	Segments                           []labSegment // a transcript's stretches, with times
	Source, Language                   string
	Translated                         bool
	Part, Parts                        int
	Bars                               []waveBar
}

// agoText is how long ago something happened, in words.
func agoText(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	switch d := now.Sub(t); {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d d ago", int(d.Hours()/24))
	}
}

// labItem describes a run for the tray, collecting its result the first
// time it is seen completed.
func (u *UI) labItem(r *http.Request, run labRun) labItemView {
	owner := u.caller(r).OwnerID()
	tool := labTargets[run.Target]
	v := labItemView{ID: run.ID, Tool: tool, Status: run.Status, Active: run.Status == "queued" || run.Status == "in_progress", At: run.CreatedAt, Ago: agoText(run.CreatedAt, time.Now())}
	note := u.notes.get(owner, run.ID)
	if note != nil {
		v.Caption, v.Prompt = note.caption, note.prompt
		if note.source != "" {
			v.Source = "/app/lab/results/" + note.source
		}
		v.Translated = note.translated
		if q := note.image; q != nil {
			v.Size, v.Look, v.LookName, v.Seed = q.Size, q.Look, lookByID(q.Look).Name, q.Seed
			for shape, sizes := range imageShapes {
				if sizes["standard"] == q.Size || sizes["large"] == q.Size {
					v.Shape = shape
				}
			}
			v.Model = q.Model
		}
	}
	if v.Model == "" {
		v.Model = run.Model
	}
	if v.Model != "" && u.d.Catalog != nil {
		if s := u.d.Catalog.ByID(v.Model); s != nil {
			v.Model = s.Name()
		}
	}
	v.Tiles = make([]int, 1)
	if note != nil && note.image != nil && note.image.N > 1 {
		v.Tiles = make([]int, note.image.N)
	}
	switch run.Status {
	case "queued":
		v.Message = "Waiting to start…"
	case "in_progress":
		v.Message = map[string]string{"image": "Making the image…", "transcribe": "Transcribing…"}[tool]
	case "cancelled":
		v.Message = "Stopped."
	case "interrupted":
		v.Message = "LocalBase restarted before this finished, and the result was not kept. Generate it again."
		if run.Reason == "memory_pressure" {
			v.Message = "This stopped because the computer ran low on memory. Try again when there is room."
		} else if run.Reason == "max_duration" {
			v.Message = "This ran for the maximum time and was stopped."
		}
	case "failed":
		v.Message = "This did not finish."
		if run.Error != nil && run.Error.Code == "model_unloading" {
			v.Message = unloadingMessage
		} else if run.Error != nil && run.Error.Message != "" {
			v.Message = run.Error.Message
		}
	case "completed":
		if note == nil {
			u.notes.put(owner, run.ID, "")
			note = u.notes.get(owner, run.ID)
		}
		// Two tabs may poll the same run: collect once.
		u.notes.collecting.Lock()
		if note.result == "" && note.text == "" && len(note.pictures) == 0 {
			u.collect(r, run, tool, note)
		}
		for _, p := range note.pictures {
			if !p.Removed {
				v.Pictures = append(v.Pictures, p)
			}
		}
		u.notes.collecting.Unlock()
		switch {
		case tool == "image" && len(v.Pictures) > 0:
			v.Image = v.Pictures[0].URL
		case note.text != "" || tool == "transcribe":
			v.Transcript, v.Segments, v.Language = note.text, note.segments, languageName(note.language)
		case note.result != "" && tool == "image":
			v.Image = "/app/lab/results/" + note.result
		case note.result != "":
			v.Audio = "/app/lab/results/" + note.result
		default:
			v.Status, v.Message = "failed", "The result is no longer available."
		}
	}
	return v
}

// collect reads a completed run's result into the Lab's result store.
func (u *UI) collect(r *http.Request, run labRun, tool string, note *labNote) {
	res := u.call(r, "GET", "/_localbase/runs/"+run.ID+"/result", "", nil)
	if res.Status != http.StatusOK {
		return
	}
	owner := u.caller(r).OwnerID()
	switch tool {
	case "image":
		var out struct {
			Data []struct {
				B64  string `json:"b64_json"`
				Seed *int64 `json:"seed"`
			} `json:"data"`
		}
		json.Unmarshal(res.Body, &out)
		for i, d := range out.Data {
			img, err := base64.StdEncoding.DecodeString(d.B64)
			if err != nil {
				continue
			}
			p := labPicture{Index: strconv.Itoa(i), URL: "/app/lab/results/" + u.results.put(owner, "image/png", img)}
			switch {
			case d.Seed != nil:
				p.Seed = *d.Seed
			case note.image != nil:
				p.Seed = note.image.Seed + int64(i)
			}
			note.pictures = append(note.pictures, p)
		}
	case "transcribe":
		var out struct {
			Text     string `json:"text"`
			Language string `json:"language"`
			Segments []struct {
				Start float64 `json:"start"`
				End   float64 `json:"end"`
				Text  string  `json:"text"`
			} `json:"segments"`
		}
		json.Unmarshal(res.Body, &out)
		note.text = strings.TrimSpace(out.Text)
		note.language = out.Language
		if note.text == "" {
			note.text = "(No speech was recognized.)"
		}
		for _, seg := range out.Segments {
			if text := strings.TrimSpace(seg.Text); text != "" {
				note.segments = append(note.segments, labSegment{Start: seg.Start, End: seg.End, Text: text})
			}
		}
	}
}

// labRuns are the person's Lab runs, newest first.
func (u *UI) labRuns(r *http.Request) []labItemView {
	res := u.call(r, "GET", "/_localbase/runs", "", nil)
	var body struct {
		Data []labRun `json:"data"`
	}
	var out []labItemView
	if res.Status == http.StatusOK && json.Unmarshal(res.Body, &body) == nil {
		for _, run := range body.Data {
			if labTargets[run.Target] != "" {
				out = append(out, u.labItem(r, run))
			}
		}
	}
	if p := u.caller(r); p != nil && p.Can("inference:video") {
		out = append(out, u.videoItems(r)...)
	}
	if p := u.caller(r); p != nil {
		out = append(out, u.speechItems(r)...)
	}
	slices.SortStableFunc(out, func(a, b labItemView) int { return b.At.Compare(a.At) })
	return out
}

// labRunGet re-renders one tray item (it polls while the run is active).
func (u *UI) labRunGet(w http.ResponseWriter, r *http.Request) {
	u.labRunRender(w, r, r.PathValue("id"))
}

func (u *UI) labRunRender(w http.ResponseWriter, r *http.Request, id string) {
	res := u.call(r, "GET", "/_localbase/runs/"+id, "", nil)
	var run labRun
	if res.Status != http.StatusOK || json.Unmarshal(res.Body, &run) != nil || labTargets[run.Target] == "" {
		// Gone (dismissed or expired): the item leaves the tray.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		return
	}
	u.fragment(w, r, "lab", "lab-item", u.labItem(r, run))
}

// labRunStop is Stop: the run is cancelled on the gateway.
func (u *UI) labRunStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	u.call(r, "POST", "/_localbase/runs/"+id+"/cancel", "", nil)
	u.labRunRender(w, r, id)
}

// labRunRemove dismisses a run and drops it from the tray.
func (u *UI) labRunRemove(w http.ResponseWriter, r *http.Request) {
	// One picture of a batch goes on its own; the last one takes the run with it.
	if note := u.notes.get(u.caller(r).OwnerID(), r.PathValue("id")); note != nil {
		u.notes.collecting.Lock()
		left := 0
		if p := note.picture(r.PostFormValue("picture")); p != nil {
			p.Removed = true
		}
		for _, p := range note.pictures {
			if !p.Removed {
				left++
			}
		}
		u.notes.collecting.Unlock()
		if left > 0 && r.PostFormValue("picture") != "" {
			u.labRunRender(w, r, r.PathValue("id"))
			return
		}
	}
	u.call(r, "DELETE", "/_localbase/runs/"+r.PathValue("id"), "", nil)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
}

// caption is a prompt shortened for the tray.
func caption(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 120 {
		return string(r[:119]) + "…"
	}
	return s
}
