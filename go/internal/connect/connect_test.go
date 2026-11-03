package connect

import (
	"encoding/json"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
)

var targets = map[string]Target{
	"nothing known": {BaseURL: "http://127.0.0.1:2273/v1"},
	"model":         {BaseURL: "http://127.0.0.1:2273/v1", Model: "some-model", Context: 32768},
	"key":           {BaseURL: "https://gateway.example/v1", Key: "lb_secret"},
	"everything":    {BaseURL: "https://gateway.example/v1", Model: "some-model", Context: 8192, Key: "lb_secret"},
}

var unfilled = regexp.MustCompile(`@[A-Z]+@`)

func TestRenderEveryClient(t *testing.T) {
	if len(clientTemplates) == 0 {
		t.Fatal("no clients")
	}
	seen := map[string]bool{}
	for name, target := range targets {
		for _, c := range Clients(target) {
			where := name + "/" + c.ID
			if seen[c.ID+name] {
				t.Errorf("%s twice", where)
			}
			seen[c.ID+name] = true
			if c.Name == "" || c.Docs == "" || !slices.Contains(Groups, c.Group) {
				t.Errorf("%s: name %q, docs %q, group %q", where, c.Name, c.Docs, c.Group)
			}
			mains := 0
			for _, s := range c.Snippets {
				if s.Main {
					mains++
				}
				if s.Label == "" || s.Text == "" {
					t.Errorf("%s: an empty snippet: %+v", where, s)
				}
				if m := unfilled.FindString(s.Text + c.Note + s.Path); m != "" {
					t.Errorf("%s: %s is left in %q", where, m, s.Text)
				}
				if s.Language == "json" {
					var v any
					if err := json.Unmarshal([]byte(s.Text), &v); err != nil {
						t.Errorf("%s: not JSON: %v\n%s", where, err, s.Text)
					}
				}
				if s.Language == "sh" {
					if out, err := exec.Command("sh", "-n", "-c", s.Text).CombinedOutput(); err != nil {
						t.Errorf("%s: not valid sh: %v %s\n%s", where, err, out, s.Text)
					}
				}
			}
			if mains != 1 {
				t.Errorf("%s: %d main snippets, want one, for the CLI's stdout", where, mains)
			}
			text := strings.Join(allText(c), "\n")
			if !strings.Contains(text, target.BaseURL) && !strings.Contains(text, strings.TrimSuffix(target.BaseURL, "/v1")) {
				t.Errorf("%s: the base URL is missing:\n%s", where, text)
			}
			// The key is the token, or the variable, until there is a key, and then it
			// never coexists with the token.
			if target.Key != "" && strings.Contains(text, KeyToken) {
				t.Errorf("%s: the token is left beside a real key", where)
			}
			if target.Key == "" && !strings.Contains(text, KeyToken) {
				t.Errorf("%s: nowhere to put the key", where)
			}
			if target.Model == "" && !strings.Contains(text, ModelToken) && c.ID != "open-webui" { // it reads /v1/models
				t.Errorf("%s: nowhere to put the model", where)
			}
		}
	}
}

// A client says whether anyone has run it. Changing this list means having run the client,
// against a gateway, with the snippet as printed.
func TestClientsSayWhetherTheyWereRun(t *testing.T) {
	run := []string{"curl", "python", "javascript"}
	for _, c := range Clients(targets["everything"]) {
		if c.Run != slices.Contains(run, c.ID) {
			t.Errorf("%s: run = %v", c.ID, c.Run)
		}
		for _, s := range c.Snippets {
			if want := map[bool]string{true: "", false: NotRunLabel}[c.Run]; s.Status != want {
				t.Errorf("%s, %q: status %q, want %q", c.ID, s.Label, s.Status, want)
			}
		}
	}
}

func allText(c Client) []string {
	out := []string{c.Note}
	for _, s := range c.Snippets {
		out = append(out, s.Text)
	}
	return out
}

// Copy never names a model: the id is data, chosen by the person (DESIGN §30.3).
func TestNoModelNamedInFixedCopy(t *testing.T) {
	cat, err := catalog.LoadWithFormer()
	if err != nil {
		t.Fatal(err)
	}
	var fixed []string
	for _, c := range Clients(Target{BaseURL: "http://127.0.0.1:2273/v1"}) {
		fixed = append(fixed, c.Name, c.Docs)
		fixed = append(fixed, allText(c)...)
	}
	for _, l := range Unusable {
		fixed = append(fixed, l.Name, l.Why)
	}
	text := strings.ToLower(strings.Join(fixed, "\n"))
	for _, s := range cat.All() {
		if strings.Contains(text, strings.ToLower(s.ModelID)) {
			t.Errorf("fixed copy names %s", s.ModelID)
		}
		if f := strings.ToLower(s.Family); f != "" && strings.Contains(text, f) {
			t.Errorf("fixed copy names the family %s", s.Family)
		}
	}
	for _, name := range []string{"gpt-", "claude", "llama", "gemini", "qwen", "mistral", "best model", "recommended"} {
		if strings.Contains(text, name) {
			t.Errorf("fixed copy says %q", name)
		}
	}
}

// A strange value stays a value: broken JSON and shell commands are the failures.
func TestStrangeValuesStayValues(t *testing.T) {
	const hostile = `a'b"c$(touch x)` + "`id`" + `\d`
	target := Target{BaseURL: "http://127.0.0.1:2273/v1", Model: hostile, Key: hostile}
	for _, c := range Clients(target) {
		for _, s := range c.Snippets {
			switch s.Language {
			case "json":
				var v any
				if err := json.Unmarshal([]byte(s.Text), &v); err != nil {
					t.Errorf("%s: %v\n%s", c.ID, err, s.Text)
				}
			case "sh":
				// Nothing it is handed may execute. Shadow every command it could run.
				script := `curl() { printf '%s\n' "$@"; }; aider() { printf '%s\n' "$@"; }; pip() { :; }; npm() { :; }; ` + s.Text
				out, err := exec.Command("sh", "-c", script).CombinedOutput()
				if err != nil {
					t.Errorf("%s: %v %s\n%s", c.ID, err, out, s.Text)
				}
			}
		}
	}
}

func TestBaseURLs(t *testing.T) {
	for _, c := range []struct {
		name, listen, origin string
		edge                 bool
		want                 []string
		note                 string
	}{
		{name: "loopback", listen: "127.0.0.1:2273", want: []string{"http://127.0.0.1:2273/v1"}},
		{name: "localhost", listen: "localhost:2273", want: []string{"http://localhost:2273/v1"}},
		{name: "ipv6 loopback", listen: "[::1]:2273", want: []string{"http://[::1]:2273/v1"}},
		{name: "wildcard", listen: "0.0.0.0:2273", want: []string{"http://127.0.0.1:2273/v1"}, note: "other addresses"},
		{name: "ipv6 wildcard", listen: "[::]:2273", want: []string{"http://127.0.0.1:2273/v1"}, note: "other addresses"},
		{name: "lan only", listen: "192.168.1.5:2273", want: []string{"http://192.168.1.5:2273/v1"}},
		{name: "public", listen: "127.0.0.1:2273", origin: "https://gateway.example",
			want: []string{"http://127.0.0.1:2273/v1", "https://gateway.example/v1"}},
		{name: "behind Access", listen: "127.0.0.1:2273", origin: "https://gateway.example", edge: true,
			want: []string{"http://127.0.0.1:2273/v1", "https://gateway.example/v1"}, note: "Cloudflare Access"},
		{name: "not an address", listen: "nonsense", origin: "https://gateway.example", want: []string{"https://gateway.example/v1"}},
	} {
		got := BaseURLs(c.listen, c.origin, c.edge)
		var urls []string
		notes := ""
		for _, b := range got {
			urls = append(urls, b.URL)
			notes += b.Note
			if b.Label == "" {
				t.Errorf("%s: no label", c.name)
			}
		}
		if !slices.Equal(urls, c.want) {
			t.Errorf("%s: %v, want %v", c.name, urls, c.want)
		}
		if c.note != "" && !strings.Contains(notes, c.note) {
			t.Errorf("%s: notes %q, want %q", c.name, notes, c.note)
		}
		if c.note == "" && notes != "" {
			t.Errorf("%s: an unexpected note: %q", c.name, notes)
		}
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://gateway.example":         "https://gateway.example/v1",
		"https://gateway.example/":        "https://gateway.example/v1",
		"https://gateway.example/v1":      "https://gateway.example/v1",
		"https://gateway.example/v1/":     "https://gateway.example/v1",
		"http://127.0.0.1:9/ai":           "http://127.0.0.1:9/ai/v1",
		" http://[::1]:2273 ":             "http://[::1]:2273/v1",
		"ftp://gateway.example":           "",
		"gateway.example":                 "",
		"https://user:pw@gateway.example": "",
		"https://gateway.example?x=1":     "",
		"":                                "",
	} {
		got, err := NormalizeBaseURL(in)
		if got != want || (want == "") != (err != nil) {
			t.Errorf("%q: %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestFind(t *testing.T) {
	target := Target{BaseURL: "http://127.0.0.1:2273/v1", Model: "m"}
	for _, id := range IDs() {
		if c := Find(id, target); c == nil || c.ID != id {
			t.Errorf("%s: %+v", id, c)
		}
	}
	if Find("nope", target) != nil {
		t.Error("an unknown client")
	}
}

// Budgets stay sane for what a client does with them.
func TestContextFigures(t *testing.T) {
	c := Find("opencode", Target{BaseURL: "http://x/v1", Model: "m", Context: 2048})
	var doc struct {
		Provider map[string]struct {
			Models map[string]struct {
				Limit struct{ Context, Output int }
			}
		}
	}
	for _, s := range c.Snippets {
		if s.Main {
			if err := json.Unmarshal([]byte(s.Text), &doc); err != nil {
				t.Fatal(err)
			}
		}
	}
	got := doc.Provider["localbase"].Models["m"].Limit
	if got.Context != 2048 || got.Output != 1024 {
		t.Errorf("small context: %+v", got)
	}
	c = Find("opencode", Target{BaseURL: "http://x/v1", Model: "m", Context: 262144})
	for _, s := range c.Snippets {
		if s.Main {
			json.Unmarshal([]byte(s.Text), &doc)
		}
	}
	if got := doc.Provider["localbase"].Models["m"].Limit; got.Output != maxOutputTokens {
		t.Errorf("large context: %+v", got)
	}
}
