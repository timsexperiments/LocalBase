package web

import (
	"net/http"
	"net/url"
	"slices"
)

// The Studio (DESIGN §22.15): a landing page of tools, each with its own
// page. The landing says what each tool makes, whether it can run here, and
// shows the person's latest results.

// studioTools are the tools with a page, in landing order.
var studioTools = []string{"image", "video", "speech", "transcribe", "decisions"}

var studioNames = map[string]string{"image": "Image", "video": "Video", "speech": "Speech", "transcribe": "Transcribe", "decisions": "Decisions"}

// studioCard is one tool on the landing.
type studioCard struct {
	Tool, Name, Line string
	Href             string
	State            string // ready | needs | denied
	Image, Last      string // the latest result: picture, or its caption
	LastAgo          string
}

// toolAvailable says whether a person may use a tool and whether it has a
// model, with the Get models filter that offers one.
func (u *UI) toolAvailable(r *http.Request, tool string) (allowed, hasModel bool, filter string) {
	p := u.caller(r)
	switch tool {
	case "image":
		return p.Can("inference:image"), len(u.labModels("image")) > 0, "images"
	case "video":
		return p.Can("inference:video"), len(u.videoModels()) > 0, "video"
	case "speech":
		return p.Can("inference:speech"), len(u.labModels("tts")) > 0, "speech"
	case "transcribe":
		return p.Can("inference:transcription"), len(u.labModels("stt")) > 0, "transcription"
	case "decisions":
		return p.Can("inference:decision"), len(u.decisionModels()) > 0, "decisions"
	}
	return false, false, ""
}

func (u *UI) studio(w http.ResponseWriter, r *http.Request) {
	items := u.labRuns(r)
	lines := map[string]string{
		"image":      "Describe a picture and get it back.",
		"video":      "A short clip from words or a picture.",
		"speech":     "Turn a script into a voice.",
		"transcribe": "Record or upload, and get the words.",
		"decisions":  "Ask a question, and see the odds.",
	}
	var cards []studioCard
	for _, tool := range studioTools {
		c := studioCard{Tool: tool, Name: studioNames[tool], Line: lines[tool], Href: "/app/studio/" + tool, State: "ready"}
		allowed, hasModel, filter := u.toolAvailable(r, tool)
		switch {
		case !allowed:
			c.State, c.Href = "denied", ""
		case !hasModel:
			c.State, c.Href = "needs", "/app/models/get?filter="+url.QueryEscape(filter)
		}
		for _, it := range items {
			if it.Tool != tool {
				continue
			}
			if c.Image == "" && it.Image != "" {
				c.Image = it.Image
			}
			if c.Last == "" && it.Caption != "" {
				c.Last, c.LastAgo = it.Caption, it.Ago
			}
		}
		cards = append(cards, c)
	}
	recent := items
	if len(recent) > 4 {
		recent = recent[:4]
	}
	u.render(w, r, "studio", map[string]any{"Cards": cards, "Recent": recent})
}

// studioTool is one tool's page.
func (u *UI) studioTool(w http.ResponseWriter, r *http.Request) {
	tool := r.PathValue("tool")
	if !slices.Contains(studioTools, tool) {
		http.NotFound(w, r)
		return
	}
	u.lab(w, r, tool)
}

// decisionModels are the selected models that answer typed questions.
func (u *UI) decisionModels() []labModel {
	var out []labModel
	for _, m := range u.labModels("llm") {
		if s := u.d.Catalog.ByID(m.ID); s != nil && s.IsDecision() {
			out = append(out, m)
		}
	}
	return out
}

// labRedirect keeps old Lab links working.
func (u *UI) labRedirect(w http.ResponseWriter, r *http.Request) {
	target := "/app/studio"
	if tab := r.URL.Query().Get("tab"); slices.Contains(studioTools, tab) {
		target += "/" + tab
	}
	http.Redirect(w, r, target, http.StatusFound)
}
