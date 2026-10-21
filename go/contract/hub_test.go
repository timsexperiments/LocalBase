package contract

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// hubFixtures serves the Hub responses tools/golden/hub.ts recorded.
func hubFixtures(t *testing.T) string {
	t.Helper()
	return hubFixtureServer(t, true)
}

// hubFixtureServer serves the recorded Hub; with strict, a request without a
// recording fails the test, otherwise it is a 404.
func hubFixtureServer(t *testing.T, strict bool) string {
	t.Helper()
	srv := rangeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sum := sha256.Sum256([]byte("https://huggingface.co" + r.URL.RequestURI()))
		b, err := os.ReadFile(filepath.Join("..", "internal", "hub", "testdata", "hub", hex.EncodeToString(sum[:])[:24]+".json"))
		if err != nil {
			if strict {
				t.Errorf("no Hub fixture for %s", r.URL.RequestURI())
			}
			http.NotFound(w, r)
			return
		}
		var f struct {
			Status int
			Body   string
		}
		json.Unmarshal(b, &f)
		w.WriteHeader(f.Status)
		w.Write([]byte(f.Body))
	}), false)
	return srv.URL
}

// TestCLIHubGolden runs `models search|add|remove-entry` through the Go CLI
// against the recorded Hub and compares output lines, data, written catalog
// files and errors with what the TS command handlers produced.
func TestCLIHubGolden(t *testing.T) {
	if *implFlag != "go" && *implFlag != "both" {
		t.Skip("runs the Go CLI")
	}
	b, err := os.ReadFile("../internal/hub/testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		CLI []struct {
			Label string          `json:"label"`
			Lines []string        `json:"lines"`
			Data  json.RawMessage `json:"data"`
			Error *string         `json:"error"`
			File  *string         `json:"file"`
		} `json:"cli"`
	}
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	endpoint := hubFixtures(t)
	dir := scratchDir(t)
	dir, _ = filepath.EvalSymlinks(dir)
	rootDir := filepath.Join(dir, "root")
	env := []string{"HF_ENDPOINT=" + endpoint, "HF_TOKEN="}
	if r := runCLIEnv(t, "go", rootDir, env, "--json", "init"); r.code != 0 {
		t.Fatalf("init: %s %s", r.stdout, r.stderr)
	}
	t.Logf("%d CLI cases", len(g.CLI))
	for _, c := range g.CLI {
		args := append([]string{"--json"}, strings.Fields(c.Label)...)
		args = append(args[:1], append([]string{"models"}, args[1:]...)...)
		if strings.HasPrefix(c.Label, "search ") {
			parts := strings.Fields(c.Label)
			args = []string{"--json", "models", "search", "--kind", parts[1], "--limit", map[string]string{"llm": "5", "stt": "5", "image": "8"}[parts[1]]}
			args = append(args, parts[2:]...)
		}
		if strings.HasPrefix(c.Label, "remove-entry whisper-base.en-q8_0") {
			// The TS run had this user model selected.
			selectSTT(t, rootDir, "whisper-base.en-q8_0")
		}
		r := runCLIEnv(t, "go", rootDir, env, args...)
		var envl struct {
			OK    bool            `json:"ok"`
			Data  json.RawMessage `json:"data"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(r.stdout), &envl); err != nil {
			t.Errorf("%s: not an envelope: %s %s", c.Label, r.stdout, r.stderr)
			continue
		}
		if c.Error != nil {
			if envl.OK || envl.Error.Message != *c.Error {
				t.Errorf("%s: got %s, want error %q", c.Label, r.stdout, *c.Error)
			}
			continue
		}
		if !envl.OK {
			t.Errorf("%s: %s", c.Label, r.stdout)
			continue
		}
		data := strings.ReplaceAll(string(envl.Data), rootDir, "<root>")
		var x, y any
		json.Unmarshal([]byte(data), &x)
		json.Unmarshal(c.Data, &y)
		if !reflect.DeepEqual(x, y) {
			t.Errorf("%s data:\n got  %s\n want %s", c.Label, data, c.Data)
		}
		lines := strings.Split(strings.TrimSuffix(strings.ReplaceAll(r.stderr, rootDir, "<root>"), "\n"), "\n")
		if strings.Join(lines, "\n") != strings.Join(c.Lines, "\n") {
			t.Errorf("%s lines:\n got\n%s\n want\n%s", c.Label, strings.Join(lines, "\n"), strings.Join(c.Lines, "\n"))
		}
		if c.File != nil {
			var path string
			json.Unmarshal(mustField(envl.Data, "path"), &path)
			got, _ := os.ReadFile(path)
			if string(got) != *c.File {
				t.Errorf("%s catalog file:\n got\n%s\n want\n%s", c.Label, got, *c.File)
			}
		}
	}
}

func mustField(raw json.RawMessage, k string) json.RawMessage {
	var m map[string]json.RawMessage
	json.Unmarshal(raw, &m)
	return m[k]
}

func selectSTT(t *testing.T, rootDir, id string) {
	t.Helper()
	st, err := store.Open(filepath.Join(rootDir, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg, err := st.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Selected["stt"] = append(cfg.Selected["stt"], id)
	if err := st.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
}

// TestDifferentialModelDiscoveryRejections: every rejection that needs no
// Hub request, on both gateways.
func TestDifferentialModelDiscoveryRejections(t *testing.T) {
	const path = "/_localbase/model-discovery"
	post := func(key func(*Target) string, body string) map[string]*Resp {
		out := map[string]*Resp{}
		for _, tg := range targets {
			h := bearer(key(tg))
			h["Content-Type"] = "application/json"
			out[tg.Name] = do(t, "POST", tg.BaseURL+path, h, body)
		}
		return out
	}
	admin := func(tg *Target) string { return tg.Admin }
	for label, body := range map[string]string{
		"not json": `{`, "array": `[]`, "no action": `{}`, "unknown action": `{"action":"delete"}`,
		"search extra key": `{"action":"search","query":"x","limit":3}`, "search bad kind": `{"action":"search","kind":"tts"}`,
		"search long query":  `{"action":"search","query":"` + strings.Repeat("q", 201) + `"}`,
		"preview short repo": `{"action":"preview","repo":"ab"}`, "preview missing repo": `{"action":"preview"}`,
		"preview bad repo": `{"action":"preview","repo":"not a repo"}`, "add bad repo": `{"action":"add","repo":"a/../b"}`,
		"add empty selector": `{"action":"add","repo":"org/repo","selector":""}`, "add install string": `{"action":"add","repo":"org/repo","install":"yes"}`,
		"add owned id":         `{"action":"add","repo":"bad repo name","modelId":"` + chatModel + `"}`,
		"recipe unknown model": `{"action":"install-recipe","modelId":"no-such-model"}`,
		"recipe without feed":  `{"action":"install-recipe","modelId":"` + chatModel + `"}`,
		"too large":            `{"action":"search","query":"` + strings.Repeat("q", 2100) + `"}`,
	} {
		sameError(t, "discovery "+label, post(admin, body))
	}
	sameError(t, "discovery as reader", post(func(tg *Target) string { return tg.Key }, `{"action":"search"}`))
	get := map[string]*Resp{}
	for _, tg := range targets {
		get[tg.Name] = do(t, "GET", tg.BaseURL+path, bearer(tg.Admin), "")
	}
	sameError(t, "discovery GET", get)
}

// TestModelDiscoveryEndToEnd drives the Go gateway's discovery against the
// recorded Hub: search, a preview equal to TS's, and an add the catalog
// picks up without a restart.
func TestModelDiscoveryEndToEnd(t *testing.T) {
	if *implFlag != "go" && *implFlag != "both" {
		t.Skip("runs the Go gateway")
	}
	b, _ := os.ReadFile("../internal/hub/testdata/golden.json")
	var g struct {
		Discovery struct {
			OK struct {
				Hits []struct{ Repo string } `json:"hits"`
			} `json:"ok"`
		} `json:"discovery"`
		Previews []struct {
			Repo    string `json:"repo"`
			Preview struct {
				OK json.RawMessage `json:"ok"`
			} `json:"preview"`
		} `json:"previews"`
	}
	json.Unmarshal(b, &g)
	tmp := scratchDir(t)
	e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp, noAuth: true, env: []string{"HF_ENDPOINT=" + hubFixtures(t), "HF_TOKEN="}}
	if r := e.cli(nil, "--json", "init"); r.code != 0 {
		t.Fatalf("init: %s %s", r.stdout, r.stderr)
	}
	e.serve("--no-auth")
	defer e.stop()
	post := func(body string) map[string]any {
		r := e.do("POST", "/_localbase/model-discovery", body)
		if r.Status != 200 {
			t.Fatalf("%s: %d %s", body, r.Status, r.Body)
		}
		return r.JSON(t)
	}
	// Search: the same repositories, in order, as TS discoverModels (the exact
	// previews are compared in hub.TestDiscoverGolden, at a fixed 64 GB).
	res := post(`{"action":"search","query":"qwen coder"}`)
	hits := res["hits"].([]any)
	var want []string
	for _, h := range g.Discovery.OK.Hits {
		want = append(want, h.Repo)
	}
	if len(hits) != len(want) {
		t.Fatalf("%d hits, want %d", len(hits), len(want))
	}
	for i, w := range want {
		if i >= len(hits) || hits[i].(map[string]any)["repo"] != w {
			t.Fatalf("search hit %d: %v, want %s", i, hits, w)
		}
	}
	if hits[0].(map[string]any)["preview"] == nil {
		t.Errorf("the top hit was not previewed: %v", hits[0])
	}
	// Preview: identical to TS for a repository the feed does not annotate,
	// apart from the fit, which depends on this host's memory (the 64 GB fit
	// is compared in hub.TestDiscoverGolden).
	res = post(`{"action":"preview","repo":"nerijs/pixel-art-xl"}`)
	got, _ := json.Marshal(res["preview"])
	for _, p := range g.Previews {
		if p.Repo == "nerijs/pixel-art-xl" {
			var x, y any
			json.Unmarshal(got, &x)
			json.Unmarshal(p.Preview.OK, &y)
			dropFits(x)
			dropFits(y)
			if !reflect.DeepEqual(x, y) {
				t.Errorf("preview:\n got  %s\n want %s", got, p.Preview.OK)
			}
		}
	}
	// Add, then the model is in the catalog the gateway serves.
	res = post(`{"action":"add","repo":"bartowski/Qwen_Qwen3.5-0.8B-GGUF"}`)
	if res["modelId"] != "qwen_qwen3.5-0.8b-q4_k_m" || res["replaced"] != false || res["operation"] != nil {
		t.Fatalf("add: %v", res)
	}
	r := e.do("GET", "/_localbase/models/qwen_qwen3.5-0.8b-q4_k_m", "")
	if r.Status != 200 || r.JSON(t)["catalog"].(map[string]any)["catalogTier"] != "user" {
		t.Fatalf("added model: %d %s", r.Status, r.Body)
	}
	if res := post(`{"action":"add","repo":"bartowski/Qwen_Qwen3.5-0.8B-GGUF"}`); res["replaced"] != true {
		t.Errorf("second add: %v", res)
	}
}

// dropFits removes each preview choice's host-dependent fit.
func dropFits(preview any) {
	m, _ := preview.(map[string]any)
	choices, _ := m["choices"].([]any)
	for _, c := range choices {
		if cm, ok := c.(map[string]any); ok {
			delete(cm, "fit")
		}
	}
}
