package system

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// `local-base connect` (DESIGN §30.5), a Go-only command: the snippet on stdout, so it can
// go into a file, everything else on stderr, and no secret unless a key was asked for.
func TestCLIConnect(t *testing.T) {
	root := filepath.Join(fsguard.ScratchDir(t), "root")
	if res := runCLI(t, root, "--json", "init"); res.code != 0 {
		t.Fatalf("init: %s", res.stderr)
	}

	// The overview: the gateway is stopped, and it says so.
	over := runCLI(t, root, "connect")
	if over.code != 0 {
		t.Fatalf("connect: %d %s", over.code, over.stderr)
	}
	for _, want := range []string{"not running", "/v1", "local-base connect <client>", "opencode", "Codex CLI"} {
		if !strings.Contains(over.stdout, want) {
			t.Errorf("overview is missing %q:\n%s", want, over.stdout)
		}
	}

	// A client's main snippet is all that stdout holds: this one is valid JSON.
	file := runCLI(t, root, "connect", "opencode", "--base-url", "https://gateway.example")
	if file.code != 0 {
		t.Fatalf("connect opencode: %d %s", file.code, file.stderr)
	}
	var doc struct {
		Provider map[string]struct {
			Options struct {
				BaseURL string `json:"baseURL"`
				APIKey  string `json:"apiKey"`
			} `json:"options"`
		} `json:"provider"`
	}
	if err := json.Unmarshal([]byte(file.stdout), &doc); err != nil {
		t.Fatalf("stdout is not the config alone: %v\n%s", err, file.stdout)
	}
	got := doc.Provider["localbase"].Options
	if got.BaseURL != "https://gateway.example/v1" || got.APIKey != "{env:LOCALBASE_API_KEY}" {
		t.Errorf("config: %+v", got)
	}
	for _, want := range []string{"export LOCALBASE_API_KEY=", "--create-key", "https://opencode.ai/", "not run"} {
		if !strings.Contains(file.stderr, want) {
			t.Errorf("stderr is missing %q:\n%s", want, file.stderr)
		}
	}

	// What has been run does not claim otherwise, and --json says which is which.
	if res := runCLI(t, root, "connect", "curl"); strings.Contains(res.stderr, "not run") {
		t.Errorf("curl has been run: %s", res.stderr)
	}
	var listing struct {
		Data struct {
			Clients []struct {
				ID  string `json:"id"`
				Run bool   `json:"run"`
			} `json:"clients"`
		} `json:"data"`
	}
	all := runCLI(t, root, "--json", "connect")
	if err := json.Unmarshal([]byte(all.stdout), &listing); err != nil {
		t.Fatalf("--json: %v\n%s", err, all.stdout)
	}
	if len(listing.Data.Clients) == 0 {
		t.Fatal("no clients")
	}
	for _, c := range listing.Data.Clients {
		if c.Run != slices.Contains([]string{"curl", "python", "javascript"}, c.ID) {
			t.Errorf("%s: run = %v", c.ID, c.Run)
		}
	}

	// Without --create-key nothing is created and nothing secret is printed.
	if keys := keyCount(t, root); keys != 0 {
		t.Fatalf("%d keys before asking", keys)
	}

	// With it, with the gateway stopped: a real key with exactly the chat scopes.
	made := runCLI(t, root, "--json", "connect", "curl", "--create-key")
	if made.code != 0 {
		t.Fatalf("create: %d %s", made.code, made.stderr)
	}
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			BaseURL string `json:"baseUrl"`
			Gateway struct {
				Running bool `json:"running"`
			} `json:"gateway"`
			Secret string `json:"secret"`
			Key    struct {
				Name   string   `json:"name"`
				Scopes []string `json:"scopes"`
			} `json:"key"`
			Clients []struct {
				ID       string `json:"id"`
				Snippets []struct {
					Text string `json:"text"`
					Main bool   `json:"main"`
				} `json:"snippets"`
			} `json:"clients"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(made.stdout), &env); err != nil {
		t.Fatalf("--json: %v\n%s", err, made.stdout)
	}
	d := env.Data
	if !env.OK || d.Secret == "" || d.Key.Name != "connect-curl" || !slices.Equal(d.Key.Scopes, []string{"inference:chat", "models:read"}) {
		t.Errorf("the key: %+v", d.Key)
	}
	if d.Gateway.Running || !strings.HasSuffix(d.BaseURL, "/v1") {
		t.Errorf("gateway %+v, base %q", d.Gateway, d.BaseURL)
	}
	if len(d.Clients) != 1 || d.Clients[0].ID != "curl" {
		t.Fatalf("clients: %+v", d.Clients)
	}
	if !strings.Contains(d.Clients[0].Snippets[0].Text, d.Secret) {
		t.Error("the key is not filled into the setup snippet")
	}
	if keys := keyCount(t, root); keys != 1 {
		t.Errorf("%d keys, want 1", keys)
	}

	// Later runs do not repeat it.
	later := runCLI(t, root, "--json", "connect", "curl")
	if strings.Contains(later.stdout+later.stderr, d.Secret) {
		t.Error("the secret came back")
	}

	// Input errors exit 2 and say what was expected.
	for name, args := range map[string][]string{
		"unknown client":      {"connect", "nope"},
		"unknown model":       {"connect", "curl", "--model", "nope"},
		"bad address":         {"connect", "curl", "--base-url", "gateway.example"},
		"scopes without flag": {"connect", "curl", "--scopes", "models:read"},
		"name without flag":   {"connect", "curl", "--name", "x"},
		"bad scope":           {"connect", "curl", "--create-key", "--scopes", "root"},
		"bad expiry":          {"connect", "curl", "--create-key", "--expires-days", "0"},
	} {
		if res := runCLI(t, root, args...); res.code != 2 {
			t.Errorf("%s: exit %d: %s", name, res.code, res.stderr)
		}
	}
	if res := runCLI(t, root, "connect", "nope"); !strings.Contains(res.stderr, "opencode") {
		t.Errorf("an unknown client should list the ids: %s", res.stderr)
	}
}

func keyCount(t *testing.T, root string) int {
	t.Helper()
	res := runCLI(t, root, "--json", "keys", "list")
	var env struct {
		Data struct {
			Keys []any `json:"keys"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(res.stdout), &env); err != nil {
		t.Fatalf("keys list: %v\n%s", err, res.stdout)
	}
	return len(env.Data.Keys)
}
