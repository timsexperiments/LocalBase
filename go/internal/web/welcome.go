package web

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
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
	// The identity provider's way back from a sign-in that failed.
	if r.URL.Query().Get("signin") == "failed" && !p.Authenticated() {
		u.renderLocalSignIn(w, http.StatusOK, localSignIn{Mode: "failed"})
		return
	}
	if r.URL.Query().Get("signedout") == "1" && !p.Authenticated() {
		u.renderLocalSignIn(w, http.StatusOK, localSignIn{Mode: "signed-out", Provider: u.d.SignInEnabled})
		return
	}
	if p.Authenticated() && p.Can("models:manage") && !u.hasChatModel() {
		http.Redirect(w, r, "/app/welcome", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/app/chat", http.StatusFound)
}

func (u *UI) welcome(w http.ResponseWriter, r *http.Request) {
	p := u.caller(r)
	u.render(w, r, "welcome", map[string]any{"Step": "hello", "FreeDisk": freeDisk(u.d.RootDir),
		"CanManage": p.Can("models:manage"), "CanAdmin": p.Can("access:read") || p.Can("keys:read"), "NoSignIn": !u.d.SignInEnabled,
		"Host": u.nameOfHost(r)})
}

// smallDownload is the stated rule behind the "Small download" label. It's about size only:
// it says nothing about how good a model is.
const smallDownload = int64(5_000_000_000)

// welcomeChoice is one chat model on the choosing page.
type welcomeChoice struct {
	CatalogRow
	TooBigDisk bool
	Small      bool
	Pick       bool
}

// welcomeChoices lists the chat models.
func (u *UI) welcomeChoices(r *http.Request) []welcomeChoice {
	rows, _ := u.catalogRows("chat")
	return chooseFrom(u.withForecast(r, rows), freeDisk(u.d.RootDir))
}

// withForecast asks the gateway whether each model that isn't installed would fit right now. A
// row keeps its total-memory answer when the gateway can't say. It's a forecast: the figure can
// change once the real file can be read, and "now" can change during a long download.
func (u *UI) withForecast(r *http.Request, rows []CatalogRow) []CatalogRow {
	for i := range rows {
		if rows[i].Installed {
			continue
		}
		var a struct {
			Admitted       bool   `json:"admitted"`
			RequestedBytes *int64 `json:"requestedBytes"`
			Refusal        *struct {
				Permanent bool   `json:"permanent"`
				Code      string `json:"code"`
			} `json:"refusal"`
		}
		if u.apiCall(r, "GET", "/_localbase/models/"+url.PathEscape(rows[i].ID)+"/admission?assumeInstalled=1", nil, &a) != "" {
			continue
		}
		switch {
		case a.Admitted:
			rows[i].Fit, rows[i].Forecast = "fits", true
		case a.Refusal != nil && a.Refusal.Permanent:
			rows[i].Fit, rows[i].Forecast = "wont-fit", true
		case a.Refusal != nil && a.Refusal.Code == "insufficient_memory":
			rows[i].Fit, rows[i].Forecast = "close-others", true
		default:
			continue
		}
		if a.RequestedBytes != nil {
			rows[i].UsesBytes = *a.RequestedBytes
		}
	}
	return rows
}

// chooseFrom picks the smallest that fits both memory and disk, so the first download is small.
func chooseFrom(rows []CatalogRow, disk int64) []welcomeChoice {
	out := make([]welcomeChoice, 0, len(rows))
	pick := -1
	for i, row := range rows {
		c := welcomeChoice{CatalogRow: row, Small: row.DownloadBytes < smallDownload, TooBigDisk: disk >= 0 && row.DownloadBytes > disk}
		if row.Fit != "wont-fit" && !c.TooBigDisk && (pick < 0 || row.DownloadBytes < out[pick].DownloadBytes) {
			pick = i
		}
		out = append(out, c)
	}
	if pick >= 0 {
		out[pick].Pick = true
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
	choices := u.welcomeChoices(r)
	picked := slices.IndexFunc(choices, func(c welcomeChoice) bool { return c.Pick })
	diskShort := len(choices) > 0 && !slices.ContainsFunc(choices, func(c welcomeChoice) bool { return !c.TooBigDisk })
	data := map[string]any{"Step": "choose", "Choices": choices, "Failure": failure, "DiskShort": diskShort,
		"SmallRule": "5 GB", "CanManage": u.caller(r).Can("models:manage")}
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
	Left        string // time left, or that it depends on the connection
	Failed      bool
	Reason      string // why it failed, in plain words
	Detail      string // the gateway's own sentence
}

// Time left is only said once the download has shown its speed.
const (
	paceAfter = 20 * time.Second
	paceBytes = int64(50_000_000)
)

// installPace is where a download was when this gateway first saw it moving.
type installPace struct {
	mu    sync.Mutex
	since map[string]paceMark
}

type paceMark struct {
	at    time.Time
	bytes int64
}

// left says how long a download will take, from its measured speed so far.
func (p *installPace) left(id string, now time.Time, done, total int64) string {
	const unknown = "How long this takes depends on your connection."
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.since == nil {
		p.since = map[string]paceMark{}
	}
	mark, ok := p.since[id]
	if !ok || done < mark.bytes {
		p.since[id] = paceMark{now, done}
		return unknown
	}
	elapsed, moved := now.Sub(mark.at), done-mark.bytes
	if elapsed < paceAfter || moved < paceBytes || total <= done {
		return unknown
	}
	seconds := float64(total-done) / (float64(moved) / elapsed.Seconds())
	switch minutes := int(math.Ceil(seconds / 60)); {
	case minutes <= 1:
		return "Less than a minute left."
	case minutes >= 120:
		return "More than two hours left."
	default:
		return fmt.Sprintf("About %d minutes left.", minutes)
	}
}

// installFailure says why a download stopped, in plain words. The gateway's sentence covers
// every cause at once, so what's checked here is the one thing that can be: the disk.
func installFailure(detail string, total, free int64) string {
	switch {
	case free >= 0 && total > free:
		return "There isn’t enough disk space. This needs " + sizeText(total) + ", and " + sizeText(free) + " is free."
	case strings.HasPrefix(detail, "Interrupted"):
		return "LocalBase stopped before this finished. It can pick up where it left off."
	}
	return "The download stopped. This is usually the connection."
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
				v.Failed, v.Detail = true, op.Detail
				v.Reason = installFailure(op.Detail, v.Total, freeDisk(u.d.RootDir))
			} else {
				v.Left = u.pace.left(id, time.Now(), v.Done, v.Total)
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
	u.render(w, r, "welcome", map[string]any{"Step": "getting", "Install": v, "Failure": msg, "Host": u.nameOfHost(r)})
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
	u.fragment(w, r, "welcome", "welcome-progress", map[string]any{"Install": v, "Failure": msg, "Host": u.nameOfHost(r)})
}

// nameOfHost is this computer's name, on its own.
func (u *UI) nameOfHost(r *http.Request) string {
	h, _ := u.readHost(r)
	return u.hostName(r, h)
}

// noAccess is the page for someone signed in with no permissions, whatever they opened.
func (u *UI) noAccess(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	w.WriteHeader(http.StatusForbidden)
	u.render(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)), "noaccess", map[string]any{})
}

// notFound is the page for an address that isn't one, in the app's own clothes.
func (u *UI) notFound(w http.ResponseWriter, r *http.Request) {
	p := u.principal(r)
	if p.Authenticated() && len(p.Permissions) == 0 {
		u.noAccess(w, r, p)
		return
	}
	w.WriteHeader(http.StatusNotFound)
	if p.Authenticated() {
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, p))
	}
	u.render(w, r, "notfound", map[string]any{})
}

// sizeText prints a download's size.
func sizeText(b int64) string {
	if b < 1e9 {
		return fmt.Sprintf("%.0f MB", float64(b)/1e6)
	}
	return fmt.Sprintf("%.1f GB", float64(b)/1e9)
}
