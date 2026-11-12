package web

import (
	"net/http"
	"net/url"
	"slices"
)

// The Studio (DESIGN §22.15, §27): a landing page of tools, each with its own
// page. The landing says what each tool makes and whether it can run here, and
// shows the person's latest results.

// studioTools are the tools with a page, in landing order.
var studioTools = []string{"image", "video", "speech", "transcribe", "decisions"}

var studioNames = map[string]string{"image": "Image", "video": "Video", "speech": "Speech", "transcribe": "Transcribe", "decisions": "Decisions"}

// studioRecentMax is how many results the landing's strip holds: enough to find
// your place, and the strip scrolls within itself.
const studioRecentMax = 6

// studioCard is one tool on the landing.
type studioCard struct {
	Tool, Name, Line string
	Href             string
	State            string // ready | needs | ask | denied
	Calm             bool   // the page's banner already says it, so the row need not repeat it
}

// installedOnly keeps the models a tool can really run. A fresh install selects default
// models before any of them has been downloaded, and selected isn't usable: a tool never
// offers one that would only fail, and its page and the landing agree.
func (u *UI) installedOnly(models []labModel) []labModel {
	var out []labModel
	for _, m := range models {
		if s := u.d.Catalog.ByID(m.ID); s != nil && u.installedBytes(s) > 0 {
			out = append(out, m)
		}
	}
	return out
}

// toolAvailable says whether a person may use a tool and whether it has a
// model, with the Get models filter that offers one.
func (u *UI) toolAvailable(r *http.Request, tool string) (allowed, hasModel bool, filter string) {
	p := u.caller(r)
	switch tool {
	case "image":
		return p.Can("inference:image"), len(u.installedOnly(u.labModels("image"))) > 0, "images"
	case "video":
		return p.Can("inference:video"), len(u.readyVideoModels()) > 0, "video"
	case "speech":
		return p.Can("inference:speech"), len(u.installedOnly(u.labModels("tts"))) > 0, "speech"
	case "transcribe":
		return p.Can("inference:transcription"), len(u.installedOnly(u.labModels("stt"))) > 0, "transcription"
	case "decisions":
		return p.Can("inference:decision"), len(u.installedOnly(u.decisionModels())) > 0, "decisions"
	}
	return false, false, ""
}

func (u *UI) studio(w http.ResponseWriter, r *http.Request) {
	items, _ := splitLost(u.labRuns(r)) // what was lost has nothing to show here
	lines := map[string]string{
		"image":      "A picture, from a few words.",
		"video":      "A short clip, from words or a picture.",
		"speech":     "A script, read in a voice.",
		"transcribe": "A recording, turned into words.",
		"decisions":  "A question, with odds you can read.",
	}
	canGet := u.caller(r).Can("models:manage")
	var cards []studioCard
	usable, missing := false, false
	for _, tool := range studioTools {
		c := studioCard{Tool: tool, Name: studioNames[tool], Line: lines[tool], Href: "/app/studio/" + tool, State: "ready"}
		allowed, hasModel, filter := u.toolAvailable(r, tool)
		switch {
		case !allowed:
			c.State, c.Href = "denied", ""
		case !hasModel && canGet:
			c.State, c.Href = "needs", "/app/models/get?filter="+url.QueryEscape(filter)
			missing = true
		case !hasModel:
			c.State, c.Href = "ask", ""
			missing = true
		default:
			usable = true
		}
		cards = append(cards, c)
	}
	if missing && !usable {
		for i := range cards {
			cards[i].Calm = true
		}
	}
	recent := items
	if len(recent) > studioRecentMax {
		recent = recent[:studioRecentMax]
	}
	u.render(w, r, "studio", map[string]any{"Cards": cards, "Recent": recent, "NoModels": missing && !usable, "CanGet": canGet})
}

// studioTile re-draws one result on the landing while it is being made. Only the
// tile changes, so the strip keeps its scroll position.
func (u *UI) studioTile(w http.ResponseWriter, r *http.Request) {
	for _, it := range u.labRuns(r) {
		if it.ID == r.PathValue("id") {
			u.fragment(w, r, "studio", "studio-tile", it)
			return
		}
	}
	// Gone (removed or expired): the tile leaves the strip.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
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
