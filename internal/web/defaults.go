package web

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
)

// The default chat model card in Settings (DESIGN §18): the ordered list of
// default chat models, edited through /_localbase/default-chat-models, with
// the model new conversations use now and why.

type defaultOption struct{ ID, Name string }

const defaultSlots = 3

func (u *UI) defaultsData(r *http.Request, notice, failure string) map[string]any {
	res := u.call(r, "GET", "/_localbase/default-chat-models", "", nil)
	var st struct {
		Models    []string `json:"models"`
		Effective struct {
			ModelID string `json:"modelId"`
			Reason  string `json:"reason"`
		} `json:"effective"`
	}
	if res.Status != 200 || json.Unmarshal(res.Body, &st) != nil {
		msg := res.apiError()
		if msg == "" {
			msg = "Could not read the default chat models."
		}
		return map[string]any{"Error": msg}
	}
	var opts []defaultOption
	if u.d.LLM != nil {
		for _, id := range u.d.LLM.Config().Selected["llm"] {
			if s := u.d.Catalog.ByID(id); s != nil && !s.IsDecision() && !s.IsEmbedding() {
				opts = append(opts, defaultOption{id, s.Name()})
			}
		}
	}
	slots := make([]string, defaultSlots)
	copy(slots, st.Models)
	name := st.Effective.ModelID
	if s := u.d.Catalog.ByID(name); s != nil {
		name = s.Name()
	}
	return map[string]any{"Options": opts, "Slots": slots, "Effective": name, "Reason": st.Effective.Reason,
		"CanManage": u.caller(r).Can("models:manage"), "Notice": notice, "Error": failure}
}

// defaultModels saves the ordered list from the card's selects.
func (u *UI) defaultModels(w http.ResponseWriter, r *http.Request) {
	var ids []string
	for i := range defaultSlots {
		if id := r.PostFormValue("default" + strconv.Itoa(i)); id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	fragment := func(notice, failure string) {
		r.Header.Set("HX-Request", "true")
		r.Header.Set("HX-Target", "default-models")
		u.render(w, r, "settings", u.defaultsData(r, notice, failure))
	}
	if len(ids) == 0 {
		fragment("", "Choose a first choice.")
		return
	}
	b, _ := json.Marshal(map[string]any{"models": ids})
	res := u.call(r, "POST", "/_localbase/default-chat-models", "application/json", b)
	if msg := res.apiError(); msg != "" || res.Status != 200 {
		if msg == "" {
			msg = "Could not save the default chat models."
		}
		fragment("", msg)
		return
	}
	fragment("Saved. New conversations use the first model that fits in memory now.", "")
}
