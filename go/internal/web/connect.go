package web

import (
	"net"
	"net/http"
	"slices"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/connect"
)

// Connect an app (DESIGN §30.4): the address, a key in one tap, and each client's
// snippets, all from internal/connect, which the CLI renders too. A key goes through
// /_localbase/api-keys as the signed-in person, so the API's rules and audit apply. The
// secret appears once, in the response to the change that made it, as in Admin.

// connectPreset is a set of scopes a key can be created with. The first is the one tap.
type connectPreset struct {
	ID, Label, Detail string
	Scopes            []string
}

var connectPresets = []connectPreset{
	{"chat", "Chat", "Chat, and list models. Enough for an editor or an agent.", []string{"inference:chat", "models:read"}},
	{"chat-embeddings", "Chat and embeddings", "Also embeddings, for apps that search your files.", []string{"inference:chat", "inference:embeddings", "models:read"}},
	{"inference", "All inference", "Every kind of request, and list models. No administration.", auth.DefaultKeyScopes},
}

// noAuthKey is what a --no-auth gateway's snippets carry: clients insist on some key.
const noAuthKey = "no-key-needed"

type connectModel struct {
	ID, Name string
	Context  int
}

type connectGroup struct {
	Label   string
	Clients []connectClient
}

type connectClient struct {
	connect.Client
	Open bool
}

func (u *UI) connectPage(w http.ResponseWriter, r *http.Request) {
	u.render(w, r, "connect", u.connectData(r, "", ""))
}

// connectKey creates a key with a preset's scopes, and shows the page with it filled in.
func (u *UI) connectKey(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("preset")
	i := slices.IndexFunc(connectPresets, func(p connectPreset) bool { return p.ID == id })
	if i < 0 {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	preset := connectPresets[i]
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		name = "Connected app (" + strings.ToLower(preset.Label) + ")"
	}
	var res struct {
		Secret string `json:"secret"`
	}
	secret, failure := "", ""
	if msg := u.apiCall(r, "POST", "/_localbase/api-keys", map[string]any{"action": "create", "name": name, "scopes": preset.Scopes}, &res); msg != "" {
		failure = msg
	} else {
		secret = res.Secret
	}
	w.Header().Set("Cache-Control", "no-store")
	r.Header.Set("HX-Request", "true")
	r.Header.Set("HX-Target", "connect-body")
	u.render(w, r, "connect", u.connectData(r, secret, failure))
}

func (u *UI) connectData(r *http.Request, secret, failure string) map[string]any {
	bases, chosenBase := u.connectBases(r)
	models := u.connectModels(r)
	target := connect.Target{BaseURL: chosenBase, Key: secret}
	if len(models) > 0 {
		chosen := models[0]
		// Only a model on the list: nothing from the URL reaches a snippet unchecked.
		if i := slices.IndexFunc(models, func(m connectModel) bool { return m.ID == r.FormValue("model") }); i >= 0 {
			chosen = models[i]
		}
		target.Model, target.Context = chosen.ID, chosen.Context
	}
	if u.d.NoAuth && secret == "" {
		target.Key = noAuthKey
	}
	open := r.FormValue("client")
	if !slices.Contains(connect.IDs(), open) {
		open = connect.IDs()[0]
	}
	var groups []connectGroup
	for _, label := range connect.Groups {
		g := connectGroup{Label: label}
		for _, c := range connect.Clients(target) {
			if c.Group == label {
				g.Clients = append(g.Clients, connectClient{c, c.ID == open})
			}
		}
		groups = append(groups, g)
	}
	p := u.caller(r)
	return map[string]any{"Bases": bases, "Base": chosenBase, "Models": models, "Model": target.Model, "Groups": groups,
		"CanCreateKey": p != nil && p.Can("keys:manage") && !u.d.NoAuth, "Presets": connectPresets, "Secret": secret, "Error": failure,
		"Unusable": connect.Unusable, "ModelToken": connect.ModelToken}
}

// connectBases is every address an app could use, and the one to start with: the
// address this page is open at, when it is one of them.
func (u *UI) connectBases(r *http.Request) ([]connect.Base, string) {
	listen := ""
	if u.d.ListenAddr != nil {
		listen = u.d.ListenAddr()
	}
	origin, edge := "", false
	if a, err := auth.LoadAccess(u.d.RootDir); err == nil && a != nil {
		origin, edge = a.Origin, a.Provider.Kind == "cloudflare-access"
	}
	bases := connect.BaseURLs(listen, origin, edge)
	page := pageBaseURL(r)
	if page != "" && !slices.ContainsFunc(bases, func(b connect.Base) bool { return b.URL == page }) {
		bases = append(bases, connect.Base{Label: "This page's address", URL: page})
	}
	if len(bases) == 0 {
		return nil, "http://" + r.Host + "/v1"
	}
	chosen := bases[0].URL
	if page != "" {
		chosen = page
	}
	// Only an address on the list, as with the model.
	if want := r.FormValue("base"); slices.ContainsFunc(bases, func(b connect.Base) bool { return b.URL == want }) {
		chosen = want
	}
	return bases, chosen
}

// pageBaseURL is the /v1 URL at the address this page is open at, or "" for a loopback
// name, which BaseURLs already covers as this computer.
func pageBaseURL(r *http.Request) string {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "" || auth.LoopbackHost(strings.Trim(host, "[]")) {
		return ""
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/v1"
}

// connectModels are the installed chat models, the default first, each with the context
// it runs with here, which clients use to budget.
func (u *UI) connectModels(r *http.Request) []connectModel {
	var out []connectModel
	for _, m := range u.chatModels(r) {
		if !m.Ready {
			continue
		}
		c := connectModel{ID: m.ID, Name: m.Name}
		if plan, err := u.d.LLM.Plan(m.ID); err == nil && plan != nil {
			c.Context = plan.Budget.EffectiveContext
		}
		out = append(out, c)
	}
	return out
}
