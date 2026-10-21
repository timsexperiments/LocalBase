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
	out = append(out, u.importedChatModels(r)...)
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

// importedChatModels are imported models (M3.5) for chat, which the API
// serves like catalog ones. One is ready when a variant has all its files.
func (u *UI) importedChatModels(r *http.Request) []chatModel {
	if u.d.Library == nil {
		return nil
	}
	entries, err := u.d.Library.Entries(r.Context())
	if err != nil {
		return nil
	}
	var out []chatModel
	for i := range entries {
		e := &entries[i]
		if !e.Imported || !e.Model.HasTask("chat") {
			continue
		}
		m := chatModel{ID: e.Model.ID, Name: e.Model.Name, Inputs: []string{"text"}, Tools: slices.Contains(e.Model.Capabilities, "tools")}
		if m.Name == "" {
			m.Name = m.ID
		}
		if slices.Contains(e.Model.Capabilities, "vision") {
			m.Vision = true
			m.Inputs = append(m.Inputs, "image")
		}
		for _, in := range u.d.Library.Inspect(e) {
			if in.Complete() {
				m.Ready = true
				break
			}
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
	u.render(w, r, "chat", map[string]any{
		"PermissionsJSON": template.JS(pb),
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
