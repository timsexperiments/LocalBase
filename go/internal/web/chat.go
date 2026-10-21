package web

import (
	"encoding/json"
	"html/template"
	"net/http"
	"path/filepath"
	"slices"

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
	Default bool     `json:"default"` // the active chat model
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
	ids := slices.Clone(cfg.Selected["llm"])
	if a := cfg.Active["llm"]; a != "" && !slices.Contains(ids, a) {
		ids = append(ids, a)
	}
	var out []chatModel
	for _, id := range ids {
		s := u.d.Catalog.ByID(id)
		if s == nil || s.Kind != "llm" || s.IsEmbedding() || s.IsDecision() {
			continue
		}
		m := chatModel{ID: id, Name: s.Name(), Ready: u.installedBytes(s) > 0, Default: id == cfg.Active["llm"]}
		if s.ContextWindowTokens != nil {
			m.Context = *s.ContextWindowTokens
		}
		caps := runtime.CapabilitiesFor(r.Context(), u.d.Store, u.d.Catalog, s, filepath.Join(u.d.RootDir, "models", s.Kind))
		m.Inputs = append([]string{}, caps.InputModalities...)
		m.Vision = slices.Contains(m.Inputs, "image")
		m.Tools = caps.Has("tool-calling")
		out = append(out, m)
	}
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
	u.render(w, r, "chat", map[string]any{
		// A JSON data block: never executed, and html/template escapes it
		// for the script context.
		"ModelsJSON": template.JS(b),
		"Ready":      ready,
		"CanChat":    p.Can("inference:chat"),
		"CanManage":  p.Can("models:manage"),
		// Dictation: the mic shows for people who may transcribe; without a
		// model it explains what to install.
		"CanDictate":     p.Can("inference:transcription"),
		"DictationModel": u.dictationModel(),
	})
}

func (u *UI) settings(w http.ResponseWriter, r *http.Request) {
	u.render(w, r, "settings", map[string]any{})
}
