package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
)

// First run (DESIGN §25): a computer with no chat model goes from hello, to choosing one, to
// its download, to a first chat. Sizes and fit are the catalog's and this machine's, never
// a guess in the browser.

// hasChatModel reports whether any chat model is fully installed.
func (u *UI) hasChatModel() bool {
	if u.d.Catalog == nil {
		return true // no catalog to choose from: nothing here can help
	}
	for _, s := range u.d.Catalog.All() {
		if t, _ := task(s); t == "Chat" && u.installedBytes(s) > 0 {
			return true
		}
	}
	return false
}

// home is where /app leads: the first-run hello for someone who can act on it, else Chat.
func (u *UI) home(w http.ResponseWriter, r *http.Request) {
	p := u.principal(r)
	if p.Authenticated() && p.Can("models:manage") && !u.hasChatModel() {
		http.Redirect(w, r, "/app/welcome", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/app/chat", http.StatusFound)
}

func (u *UI) welcome(w http.ResponseWriter, r *http.Request) {
	p := u.caller(r)
	u.render(w, r, "welcome", map[string]any{"Step": "hello", "FreeDisk": freeDisk(u.d.RootDir),
		"CanManage": p.Can("models:manage"), "CanAdmin": p.Can("access:read") || p.Can("keys:read"), "NoSignIn": !u.d.SignInEnabled})
}

// welcomeChoice is one chat model on the choosing page.
type welcomeChoice struct {
	CatalogRow
	TooBigDisk bool
	Pick       bool
}

// welcomeChoices lists the chat models. The first that fits both memory and disk is picked.
func (u *UI) welcomeChoices() []welcomeChoice {
	rows, _ := u.catalogRows("chat")
	disk := freeDisk(u.d.RootDir)
	out := make([]welcomeChoice, 0, len(rows))
	picked := false
	for _, row := range rows {
		c := welcomeChoice{CatalogRow: row, TooBigDisk: disk >= 0 && row.DownloadBytes > disk}
		if !picked && row.Fit != "wont-fit" && !c.TooBigDisk {
			c.Pick, picked = true, true
		}
		out = append(out, c)
	}
	return out
}

func (u *UI) welcomeChoose(w http.ResponseWriter, r *http.Request) {
	if id := u.downloading(r); id != "" {
		http.Redirect(w, r, "/app/welcome/getting?model="+url.QueryEscape(id), http.StatusFound)
		return
	}
	u.renderChoose(w, r, "")
}

// downloading names a model being installed right now, or "".
func (u *UI) downloading(r *http.Request) string {
	var res struct {
		Models []struct {
			ID        string `json:"id"`
			Operation *struct {
				Action string `json:"action"`
				State  string `json:"state"`
			} `json:"operation"`
		} `json:"models"`
	}
	if u.apiCall(r, "GET", "/_localbase/model-management", nil, &res) != "" {
		return ""
	}
	for _, m := range res.Models {
		if op := m.Operation; op != nil && op.Action == "install" && op.State == "running" {
			return m.ID
		}
	}
	return ""
}

func (u *UI) renderChoose(w http.ResponseWriter, r *http.Request, failure string) {
	choices := u.welcomeChoices()
	picked := slices.IndexFunc(choices, func(c welcomeChoice) bool { return c.Pick })
	data := map[string]any{"Step": "choose", "Choices": choices, "Failure": failure,
		"CanManage": u.caller(r).Can("models:manage")}
	if picked >= 0 {
		data["Picked"] = choices[picked]
	}
	u.render(w, r, "welcome", data)
}

// welcomeGet starts the download of the chosen model.
func (u *UI) welcomeGet(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("modelId")
	// Already on this computer: all it needs is turning on.
	if s := u.d.Catalog.ByID(id); s != nil && u.installedBytes(s) > 0 {
		loadPage(w, r, u.finishInstall(r, id))
		return
	}
	var op struct{ State, Detail string }
	if msg := u.apiCall(r, "POST", "/_localbase/model-management", map[string]any{"modelId": id, "action": "install"}, &op); msg != "" {
		u.renderChoose(w, r, msg)
		return
	}
	if op.State == "failed" {
		u.renderChoose(w, r, op.Detail)
		return
	}
	loadPage(w, r, "/app/welcome/getting?model="+url.QueryEscape(id))
}

// loadPage sends htmx to a real page load. Chat and the download need their own scripts, which
// a swapped-in body would not start.
func loadPage(w http.ResponseWriter, r *http.Request, path string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", path)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// welcomeInstall is a model's download as the gateway reports it.
type welcomeInstall struct {
	ID, Name    string
	Installed   bool
	State       string
	Done, Total int64
	Word        string
	Failed      bool
}

// installWords says what a download is doing, from the installer's own events.
func installWords(detail string) string {
	switch detail {
	case "download-progress", "download-started", "download-completed":
		return "Downloading."
	case "verification-started", "verification-completed":
		return "Checking each file as it arrives."
	case "Preparing installation":
		return "Getting started."
	}
	return "Working on it."
}

func (u *UI) readInstall(r *http.Request, id string) (welcomeInstall, string) {
	var res struct {
		Models []struct {
			ID        string `json:"id"`
			Installed bool   `json:"installed"`
			Operation *struct {
				State           string `json:"state"`
				Detail          string `json:"detail"`
				DownloadedBytes *int64 `json:"downloadedBytes"`
				TotalBytes      *int64 `json:"totalBytes"`
			} `json:"operation"`
		} `json:"models"`
	}
	v := welcomeInstall{ID: id, Name: id}
	if s := u.d.Catalog.ByID(id); s != nil {
		v.Name = s.Name()
	}
	if msg := u.apiCall(r, "GET", "/_localbase/model-management", nil, &res); msg != "" {
		return v, msg
	}
	for _, m := range res.Models {
		if m.ID != id {
			continue
		}
		v.Installed = m.Installed
		if op := m.Operation; op != nil {
			v.State, v.Word = op.State, installWords(op.Detail)
			if op.DownloadedBytes != nil {
				v.Done = *op.DownloadedBytes
			}
			if op.TotalBytes != nil {
				v.Total = *op.TotalBytes
			}
			if op.State == "failed" {
				v.Failed, v.Word = true, op.Detail
			}
		}
	}
	return v, ""
}

// finishInstall makes the finished model the one new chats use, and says where to go.
func (u *UI) finishInstall(r *http.Request, id string) string {
	// The person chose this one, so it's the default. Activate also enables, and is safe to
	// repeat if the gateway already made it the default. A gateway from before that (DESIGN
	// §13.6) refuses until it's enabled, so that's the one retry. A failure here leaves it
	// installed, and the Library can still turn it on.
	send := func(action string) string {
		var ignored json.RawMessage
		return u.apiCall(r, "POST", "/_localbase/model-management", map[string]any{"modelId": id, "action": action}, &ignored)
	}
	if send("activate") != "" && (send("enable") != "" || send("activate") != "") {
		return "/app/models"
	}
	return "/app/chat?ready=1"
}

func (u *UI) welcomeGetting(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("model")
	v, msg := u.readInstall(r, id)
	if msg == "" && v.Installed {
		http.Redirect(w, r, u.finishInstall(r, id), http.StatusSeeOther)
		return
	}
	u.render(w, r, "welcome", map[string]any{"Step": "getting", "Install": v, "Failure": msg})
}

// welcomeProgress is the download's refresh.
func (u *UI) welcomeProgress(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("model")
	v, msg := u.readInstall(r, id)
	if msg == "" && v.Installed {
		w.Header().Set("HX-Redirect", u.finishInstall(r, id))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	u.fragment(w, r, "welcome", "welcome-progress", map[string]any{"Install": v, "Failure": msg})
}

// notFound is the page for an address that isn't one, in the app's own clothes.
func (u *UI) notFound(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	if p := u.principal(r); p.Authenticated() {
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, p))
	}
	u.render(w, r, "notfound", map[string]any{})
}
