package web

import (
	"encoding/json"
	"html/template"
	"net/http"
	"path/filepath"
	"slices"

	"github.com/timsexperiments/LocalBase/go/internal/library"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
)

// Chat (§15 step 1, .design/research/chat/BRIEF.md): the page is rendered
// here, with the person's chat models as a JSON data block; static/js/chat.js
// streams replies through /app/api/v1/chat/completions as the signed-in
// person, so the API applies its own permissions and limits. Conversations
// stay on the device (TS's history key and schema), never on the server.

// chatModel is one chat model the picker offers.
type chatModel struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Ready   bool     `json:"ready"`   // installed on this machine
	Default bool     `json:"default"` // the effective default chat model (DESIGN §18)
	Vision  bool     `json:"vision"`  // accepts images
	Inputs  []string `json:"inputs"`  // effective input modalities (attachments)
	Tools   bool     `json:"tools"`   // calls tools
	Context int      `json:"context,omitempty"`
}

// chatModels are the selected chat models (not embedding or decision
// models), ready ones first and the default first among them.
func (u *UI) chatModels(r *http.Request) []chatModel {
	if u.d.LLM == nil {
		return nil
	}
	cfg := u.d.LLM.Config()
	def := u.d.LLM.DefaultModel(true) // a page load starts a new conversation
	cands := library.ChatCandidates(r.Context(), cfg, u.d.Catalog, filepath.Join(u.d.RootDir, "models", "llm"), u.d.Library)
	var out []chatModel
	for _, c := range cands {
		s := c.Spec
		if s == nil {
			continue
		}
		m := chatModel{ID: c.ID, Name: s.Name(), Ready: c.Ready, Default: c.ID == def.ModelID}
		if s.ContextWindowTokens != nil {
			m.Context = *s.ContextWindowTokens
		}
		caps := runtime.CapabilitiesFor(r.Context(), u.d.Store, u.d.Catalog, s, filepath.Join(u.d.RootDir, "models", s.Kind))
		m.Inputs = append([]string{}, caps.InputModalities...)
		m.Vision = slices.Contains(m.Inputs, "image")
		m.Tools = caps.Has("tool-calling")
		out = append(out, m)
	}
	out = append(out, importedChatModels(cands)...)
	slices.SortStableFunc(out, func(a, b chatModel) int {
		rank := func(m chatModel) int {
			switch {
			case m.Ready && m.Default:
				return 0
			case m.Ready:
				return 1
			}
			return 2
		}
		return rank(a) - rank(b)
	})
	return out
}

// importedChatModels are the imported models (M3.5) among cands, which the API serves like catalog
// ones. One is ready when a variant has all its files.
func importedChatModels(cands []library.ChatCandidate) []chatModel {
	var out []chatModel
	for _, c := range cands {
		e := c.Entry
		if e == nil {
			continue
		}
		m := chatModel{ID: e.Model.ID, Name: e.Model.Name, Inputs: []string{"text"}, Tools: slices.Contains(e.Model.Capabilities, "tools"), Ready: c.Ready}
		if m.Name == "" {
			m.Name = m.ID
		}
		if slices.Contains(e.Model.Capabilities, "vision") {
			m.Vision = true
			m.Inputs = append(m.Inputs, "image")
		}
		out = append(out, m)
	}
	return out
}

// dictationModel is the installed, selected transcription model dictation
// uses: the active one when installed, otherwise the first (TS uses the
// first available).
func (u *UI) dictationModel() string {
	if u.d.LLM == nil {
		return ""
	}
	cfg := u.d.LLM.Config()
	ids := slices.Clone(cfg.Selected["stt"])
	if a := cfg.Active["stt"]; a != "" {
		ids = append([]string{a}, ids...)
	}
	for _, id := range ids {
		if s := u.d.Catalog.ByID(id); s != nil && s.Kind == "stt" && u.installedBytes(s) > 0 {
			return id
		}
	}
	return ""
}

// chatDefault is the effective default chat model for a new conversation,
// with the reason when it is not the preferred one (DESIGN §18). Each call
// is a decision point.
func (u *UI) chatDefault(w http.ResponseWriter, r *http.Request) {
	var c runtime.DefaultChoice
	if u.d.LLM != nil {
		c = u.d.LLM.DefaultModel(true)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(c)
}

func (u *UI) chat(w http.ResponseWriter, r *http.Request) {
	p := u.caller(r)
	models := u.chatModels(r)
	if models == nil {
		models = []chatModel{}
	}
	b, _ := json.Marshal(models)
	ready := 0
	for _, m := range models {
		if m.Ready {
			ready++
		}
	}
	// The generation permissions this person holds: tools are offered only
	// for these (the API enforces them regardless).
	perms := []string{}
	for _, perm := range []string{"inference:image", "inference:video", "inference:speech"} {
		if p.Can(perm) {
			perms = append(perms, perm)
		}
	}
	pb, _ := json.Marshal(perms)
	var def runtime.DefaultChoice
	if u.d.LLM != nil {
		def = u.d.LLM.DefaultModel(false) // the choice chatModels just made
	}
	db, _ := json.Marshal(def)
	u.render(w, r, "chat", map[string]any{
		"PermissionsJSON": template.JS(pb), //nolint:gosec // json.Marshal output: it escapes < > &
		"DefaultJSON":     template.JS(db), //nolint:gosec // json.Marshal output: it escapes < > &
		// A JSON data block: never executed, and html/template escapes it
		// for the script context.
		"ModelsJSON":  template.JS(b), //nolint:gosec // json.Marshal output: it escapes < > &
		"Ready":       ready,
		"FirstChat":   r.URL.Query().Get("ready") == "1",
		"Downloading": ready == 0 && u.downloading(r) != "",
		"CanChat":     p.Can("inference:chat"),
		"CanManage":   p.Can("models:manage"),
		// Dictation: the mic shows for people who may transcribe; without a
		// model it explains what to install.
		"CanDictate":     p.Can("inference:transcription"),
		"DictationModel": u.dictationModel(),
	})
}
