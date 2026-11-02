// Package connect says how to point an app at this gateway: its base URLs, and
// each client's snippets. The page and the CLI both render from here, so they
// cannot disagree (DESIGN §28.3). It knows nothing about the gateway.
package connect

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

const (
	// KeyToken stands for the key wherever a snippet must hold it literally.
	KeyToken = "YOUR_LOCALBASE_KEY"
	// ModelToken stands for the model when none is known yet.
	ModelToken = "YOUR_MODEL_ID"
	// KeyEnv is the variable snippets read the key from.
	KeyEnv = "LOCALBASE_API_KEY"
	// NotRunLabel marks what has only been compared with a client's own documentation.
	NotRunLabel = "Follows the client's docs, not run"

	// fallbackContext is the reference launch's context, for a model whose real
	// figure is not known. Clients use it to budget, so too small is the safe side.
	fallbackContext = 8192
	maxOutputTokens = 4096 // the gateway's own default for max_tokens
)

// Target is what a snippet points at.
type Target struct {
	BaseURL string // ends in /v1
	Model   string // "" prints ModelToken
	Context int    // the model's effective context here; 0 when unknown
	Key     string // "" prints KeyToken
}

// Field is one value a person types into a client's settings.
type Field struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Snippet is one thing to copy. Main marks the one that does the job, which the
// CLI prints on stdout so it can be redirected into a file.
type Snippet struct {
	Label    string  `json:"label"`
	Path     string  `json:"path,omitempty"` // where it goes, when it is a file
	Language string  `json:"language"`       // sh, python, javascript, json, yaml, text
	Text     string  `json:"text"`
	Main     bool    `json:"main"`
	Fields   []Field `json:"fields,omitempty"`
	// Status is NotRunLabel on a client nobody has run these with.
	Status string `json:"status,omitempty"`
}

// Client is one app, with what its own documentation says today.
type Client struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Group string `json:"group"`
	Docs  string `json:"docs"`
	Note  string `json:"note,omitempty"`
	// Run: these snippets have been run for real against a gateway. Everything else only
	// follows the client's documentation, and says so wherever it is shown.
	Run      bool      `json:"run"`
	Snippets []Snippet `json:"snippets"`
}

// The groups, in the order they are shown.
const (
	GroupCode   = "Code"
	GroupAgents = "Editors and agents"
	GroupApps   = "Apps"
)

// Groups lists them in order.
var Groups = []string{GroupCode, GroupAgents, GroupApps}

// Base is an address an app can use.
type Base struct {
	Label string `json:"label"`
	URL   string `json:"url"`
	Note  string `json:"note,omitempty"`
}

// BaseURLs reads the addresses from where the gateway listens and from the public
// origin browser sign-in is configured with (""), if any. edgeAccess: that origin is
// behind Cloudflare Access, which answers before the gateway does.
func BaseURLs(listen, origin string, edgeAccess bool) []Base {
	var out []Base
	if host, port, err := net.SplitHostPort(listen); err == nil {
		ip := net.ParseIP(host)
		wildcard := host == "" || (ip != nil && ip.IsUnspecified())
		note := ""
		if wildcard {
			host = "127.0.0.1"
			note = "It also listens on this computer's other addresses. From another device, use the address you reach this computer by."
		}
		out = append(out, Base{Label: "This computer", URL: "http://" + net.JoinHostPort(host, port) + "/v1", Note: note})
	}
	if origin != "" {
		b := Base{Label: "Your public address", URL: strings.TrimSuffix(origin, "/") + "/v1"}
		if edgeAccess {
			b.Note = "Cloudflare Access answers this address before LocalBase does, so an app also has to get past it: a service token, or a bypass rule for /v1/*. LocalBase still checks the key."
		}
		out = append(out, b)
	}
	return out
}

// NormalizeBaseURL takes an address a person gave and returns its /v1 URL.
func NormalizeBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("expected an http or https address, such as https://example.com or https://example.com/v1")
	}
	p := strings.TrimSuffix(u.EscapedPath(), "/")
	if !strings.HasSuffix(p, "/v1") {
		p += "/v1"
	}
	return u.Scheme + "://" + u.Host + p, nil
}

// escape makes a value safe where a template places it. Every template puts values
// inside double quotes (JSON, Python, JavaScript, YAML) or single quotes (sh), and sh
// bodies are JSON too. Ids reach here validated; this is so a strange one still cannot
// turn into a command or broken JSON.
func escape(language, v string) string {
	b, _ := json.Marshal(v)
	s := strings.TrimSuffix(strings.TrimPrefix(string(b), `"`), `"`)
	if language == "sh" {
		s = strings.ReplaceAll(s, `'`, `'\''`)
	}
	return s
}

// fill replaces the template's marks.
func fill(language, text string, t Target) string {
	model, key := t.Model, t.Key
	if model == "" {
		model = ModelToken
	}
	if key == "" {
		key = KeyToken
	}
	context := t.Context
	if context <= 0 {
		context = fallbackContext
	}
	return strings.NewReplacer(
		"@BASE@", escape(language, t.BaseURL), "@ROOT@", escape(language, strings.TrimSuffix(t.BaseURL, "/v1")),
		"@MODEL@", escape(language, model), "@KEY@", escape(language, key), "@ENV@", KeyEnv,
		"@CONTEXT@", strconv.Itoa(context), "@OUTPUT@", strconv.Itoa(min(maxOutputTokens, context/2)),
	).Replace(text)
}

// Clients renders every client for a target, in display order.
func Clients(t Target) []Client {
	out := make([]Client, 0, len(clientTemplates))
	for _, c := range clientTemplates {
		out = append(out, render(c, t))
	}
	return out
}

// Find renders one client, or nil.
func Find(id string, t Target) *Client {
	for _, c := range clientTemplates {
		if c.ID == id {
			r := render(c, t)
			return &r
		}
	}
	return nil
}

// IDs lists the client ids in display order.
func IDs() []string {
	out := make([]string, 0, len(clientTemplates))
	for _, c := range clientTemplates {
		out = append(out, c.ID)
	}
	return out
}

func render(c Client, t Target) Client {
	out := c
	out.Note = fill("text", c.Note, t)
	if strings.HasPrefix(c.Docs, "/") { // this gateway's own
		out.Docs = strings.TrimSuffix(t.BaseURL, "/v1") + c.Docs
	}
	out.Snippets = make([]Snippet, 0, len(c.Snippets))
	for _, s := range c.Snippets {
		r := s
		r.Path = fill("text", s.Path, t)
		if !c.Run {
			r.Status = NotRunLabel
		}
		if len(s.Fields) > 0 {
			r.Fields = make([]Field, 0, len(s.Fields))
			lines := make([]string, 0, len(s.Fields))
			for _, f := range s.Fields {
				v := fillPlain(f.Value, t)
				r.Fields = append(r.Fields, Field{f.Label, v})
				lines = append(lines, f.Label+": "+v)
			}
			r.Text = strings.Join(lines, "\n")
		} else {
			r.Text = fill(s.Language, s.Text, t)
		}
		out.Snippets = append(out.Snippets, r)
	}
	return out
}

// fillPlain is for a value typed into a settings field: nothing is escaped.
func fillPlain(text string, t Target) string {
	model, key := t.Model, t.Key
	if model == "" {
		model = ModelToken
	}
	if key == "" {
		key = KeyToken
	}
	return strings.NewReplacer("@BASE@", t.BaseURL, "@MODEL@", model, "@KEY@", key).Replace(text)
}
