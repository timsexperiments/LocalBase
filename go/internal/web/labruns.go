package web

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
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
	"/v1/audio/speech":         "speech",
	"/v1/audio/transcriptions": "transcribe",
}

// labRun is the part of a run object the tray uses.
type labRun struct {
	ID, Target, Status, Reason string
	Error                      *struct{ Code, Message string } `json:"error"`
	CreatedAt                  time.Time
}

// unloadingMessage: a request reached a model while someone unloaded it
// (DESIGN §23); the gateway answers 503 model_unloading.
const unloadingMessage = "The model was being unloaded, so this didn’t run. Try again: it loads again when needed."

// labNote is what the tray remembers about a run, in memory, bound to its
// owner: the prompt as a caption, and the collected result.
type labNote struct {
	owner, caption, prompt, result, text string
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
func (u *UI) labStart(w http.ResponseWriter, r *http.Request, path, ctype, prompt string, body []byte) bool {
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
	w.Header().Set("HX-Retarget", "#lab-items")
	w.Header().Set("HX-Reswap", "afterbegin")
	u.fragment(w, r, "lab", "lab-item", u.labItem(r, run))
	return true
}

// labItemView is one tray item.
type labItemView struct {
	ID, Tool, Status, Caption, Message string
	Prompt                             string // to try again (empty for a file)
	Image, Audio, Transcript           string
	Active                             bool
}

// labItem describes a run for the tray, collecting its result the first
// time it is seen completed.
func (u *UI) labItem(r *http.Request, run labRun) labItemView {
	owner := u.caller(r).OwnerID()
	tool := labTargets[run.Target]
	v := labItemView{ID: run.ID, Tool: tool, Status: run.Status, Active: run.Status == "queued" || run.Status == "in_progress"}
	note := u.notes.get(owner, run.ID)
	if note != nil {
		v.Caption, v.Prompt = note.caption, note.prompt
	}
	switch run.Status {
	case "queued":
		v.Message = "Waiting to start…"
	case "in_progress":
		v.Message = map[string]string{"image": "Making the image…", "speech": "Speaking…", "transcribe": "Transcribing…"}[tool]
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
		if note.result == "" && note.text == "" {
			u.collect(r, run, tool, note)
		}
		u.notes.collecting.Unlock()
		switch {
		case note.text != "" || tool == "transcribe":
			v.Transcript = note.text
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
				B64 string `json:"b64_json"`
			} `json:"data"`
		}
		if json.Unmarshal(res.Body, &out) == nil && len(out.Data) > 0 {
			if img, err := base64.StdEncoding.DecodeString(out.Data[0].B64); err == nil {
				note.result = u.results.put(owner, "image/png", img)
			}
		}
	case "speech":
		note.result = u.results.put(owner, "audio/wav", res.Body)
	case "transcribe":
		var out struct {
			Text string `json:"text"`
		}
		json.Unmarshal(res.Body, &out)
		note.text = strings.TrimSpace(out.Text)
		if note.text == "" {
			note.text = "(No speech was recognized.)"
		}
	}
}

// labRuns are the person's Lab runs, newest first.
func (u *UI) labRuns(r *http.Request) []labItemView {
	res := u.call(r, "GET", "/_localbase/runs", "", nil)
	var body struct {
		Data []labRun `json:"data"`
	}
	if res.Status != http.StatusOK || json.Unmarshal(res.Body, &body) != nil {
		return nil
	}
	var out []labItemView
	for _, run := range body.Data {
		if labTargets[run.Target] != "" {
			out = append(out, u.labItem(r, run))
		}
	}
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
