package web

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
)

// Settings (§15 step 3): chat history on this device (settings.js), the
// account, and, for people who manage models, the Hugging Face token. The
// token goes through /_localbase/hf-token as the signed-in person, so the
// API's checks apply; the page never shows a saved token.

// permissionLabels name what each permission lets a person do, in order.
var permissionLabels = []struct{ id, label string }{
	{"inference:chat", "Chat"},
	{"inference:image", "Create images"},
	{"inference:video", "Create video"},
	{"inference:speech", "Create speech"},
	{"inference:transcription", "Transcribe audio"},
	{"inference:embeddings", "Embeddings"},
	{"inference:decision", "Decisions"},
	{"models:read", "See models"},
	{"models:manage", "Manage models"},
	{"keys:read", "See API keys"},
	{"keys:manage", "Manage API keys"},
	{"access:read", "See access"},
	{"access:manage", "Manage access"},
	{"sessions:read", "See sessions"},
	{"sessions:revoke", "Sign out sessions"},
	{"configuration:read", "See configuration"},
	{"configuration:manage", "Change configuration"},
}

// hfStatus is the API's token status (never the token itself).
type hfStatus struct {
	Configured   bool    `json:"configured"`
	Source       *string `json:"source"`
	EnvAvailable bool    `json:"envAvailable"`
	Check        *struct {
		State   string  `json:"state"`
		Name    *string `json:"name"`
		Role    *string `json:"role"`
		Message string  `json:"message"`
	} `json:"check"`
}

func (s *hfStatus) source() string {
	if s == nil || s.Source == nil {
		return ""
	}
	return *s.Source
}

// describe is TS describeCheck.
func (s *hfStatus) describe() string {
	c := s.Check
	switch c.State {
	case "invalid":
		return "Hugging Face rejected this token. Save a fresh one."
	case "unreachable":
		return c.Message
	}
	who, role := "", ""
	if c.Name != nil && *c.Name != "" {
		who = " for " + *c.Name
	}
	if c.Role != nil && *c.Role != "" {
		role = " (" + *c.Role + " access)"
	}
	return "Hugging Face accepted the token" + who + role + "."
}

func (u *UI) settings(w http.ResponseWriter, r *http.Request) {
	p := u.caller(r)
	var can []string
	for _, l := range permissionLabels {
		if p.Can(l.id) {
			can = append(can, l.label)
		}
	}
	data := map[string]any{"Can": strings.Join(can, " · ")}
	if p.Can("models:manage") {
		data["Token"] = u.tokenData(r, nil, "", "")
		data["ComputerName"] = u.computerNameData(r, "", "")
	}
	if p.Can("models:read") && u.d.LLM != nil {
		data["Defaults"] = u.defaultsData(r, "", "")
	}
	u.render(w, r, "settings", data)
}

// tokenData reads the status (unless given) for the token card.
func (u *UI) tokenData(r *http.Request, status *hfStatus, notice, failure string) map[string]any {
	if status == nil {
		res := u.call(r, "GET", "/_localbase/hf-token", "", nil)
		if msg := res.apiError(); msg != "" || res.Status != 200 {
			if msg == "" {
				msg = "Could not read the token status."
			}
			return map[string]any{"Error": msg}
		}
		status = &hfStatus{}
		if json.Unmarshal(res.Body, status) != nil {
			return map[string]any{"Error": "Could not read the token status."}
		}
	}
	return map[string]any{"Status": status, "Source": status.source(), "Notice": notice, "Error": failure}
}

// hfToken saves, checks or clears the token (TS HfTokenPanel actions).
func (u *UI) hfToken(w http.ResponseWriter, r *http.Request) {
	action := r.PostFormValue("action")
	body := map[string]any{"action": action}
	switch action {
	case "save":
		token := strings.TrimSpace(r.PostFormValue("token"))
		if token == "" {
			u.tokenFragment(w, r, u.tokenData(r, nil, "", "Enter a token to save."))
			return
		}
		body["token"] = token
	case "validate", "clear":
	default:
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	b, _ := json.Marshal(body)
	res := u.call(r, "POST", "/_localbase/hf-token", "application/json", b)
	if msg := res.apiError(); msg != "" || res.Status != 200 {
		if msg == "" {
			msg = "Could not update the token."
		}
		u.tokenFragment(w, r, u.tokenData(r, nil, "", msg))
		return
	}
	next := &hfStatus{}
	if json.Unmarshal(res.Body, next) != nil {
		u.tokenFragment(w, r, u.tokenData(r, nil, "", "Could not update the token."))
		return
	}
	notice := "Saved the token."
	switch {
	case next.Check != nil:
		notice = next.describe()
	case action == "clear" && next.source() == "env":
		notice = "Cleared the saved token; LocalBase now uses HF_TOKEN."
	case action == "clear":
		notice = "Cleared the saved token."
	}
	u.tokenFragment(w, r, u.tokenData(r, next, notice, ""))
}

func (u *UI) tokenFragment(w http.ResponseWriter, r *http.Request, token map[string]any) {
	r.Header.Set("HX-Request", "true")
	r.Header.Set("HX-Target", "hf-token")
	u.render(w, r, "settings", token)
}

// computerNameData is the Computer name setting: the name the owner gave this computer, if any,
// and what it's called without one.
func (u *UI) computerNameData(r *http.Request, notice, failure string) map[string]any {
	var host struct {
		Host struct {
			Name       *string `json:"name"`
			NameSource string  `json:"nameSource"`
		} `json:"host"`
	}
	u.apiCall(r, "GET", "/_localbase/host", nil, &host)
	name := ""
	if host.Host.Name != nil && host.Host.NameSource == "setting" {
		name = *host.Host.Name
	}
	fallback := computerName()
	if host.Host.Name != nil && host.Host.NameSource == "system" && strings.TrimSpace(os.Getenv("LOCALBASE_COMPUTER_NAME")) == "" {
		fallback = *host.Host.Name
	}
	return map[string]any{"Name": name, "Fallback": fallback, "Notice": notice, "Failure": failure}
}

// computerNameSave saves or resets the name shown for this computer.
func (u *UI) computerNameSave(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	if r.PostFormValue("action") == "reset" {
		name = ""
	}
	notice, failure := "Saved.", ""
	if name == "" {
		notice = "It uses this computer's own name."
	}
	var out json.RawMessage
	if msg := u.apiCall(r, "PUT", "/_localbase/computer-name", map[string]any{"name": name}, &out); msg != "" {
		notice, failure = "", msg
	}
	u.fragment(w, r, "settings", "computer-name", u.computerNameData(r, notice, failure))
}
