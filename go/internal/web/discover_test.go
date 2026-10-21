package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
)

// fakeDiscovery answers /_localbase/model-discovery like the gateway.
type fakeDiscovery struct {
	sent  []map[string]any
	reply map[string]string // action -> JSON body
	fail  map[string]string // action -> error JSON (status 400)
}

func (f *fakeDiscovery) call(_ context.Context, p *auth.Principal, method, path, _ string, body []byte) APIResult {
	if path != "/_localbase/model-discovery" || method != "POST" || !p.Can("models:manage") {
		return APIResult{Status: 403, Body: []byte(`{"error":{"message":"forbidden"}}`)}
	}
	var in map[string]any
	json.Unmarshal(body, &in)
	f.sent = append(f.sent, in)
	action, _ := in["action"].(string)
	if e, ok := f.fail[action]; ok {
		return APIResult{Status: 400, Body: []byte(e)}
	}
	return APIResult{Status: 200, Body: []byte(f.reply[action])}
}

const searchReply = `{"cached":true,"hits":[
 {"repo":"org/Good-Model-GGUF","url":"https://huggingface.co/org/Good-Model-GGUF","downloads":12345,"license":"apache-2.0","pipelineTag":"text-generation","gated":false,"likely":{"kind":"llm","label":"llm"},
  "preview":{"kind":"llm","defaultPath":"m-Q4_K_M.gguf","choices":[
   {"path":"m-IQ2_XS.gguf","sizeBytes":1000000000,"partCount":1,"quant":"IQ2_XS","modelId":"good-iq2","existing":null,"fit":{"verdict":"fits","minVramGb":2,"message":"Leaves room"}},
   {"path":"m-Q4_K_M.gguf","sizeBytes":2000000000,"partCount":1,"quant":"Q4_K_M","modelId":"good-q4","existing":null,"fit":{"verdict":"fits","minVramGb":3,"message":"Leaves room"}},
   {"path":"m-Q8_0.gguf","sizeBytes":4000000000,"partCount":2,"quant":"Q8_0","modelId":"good-q8","existing":"built-in","fit":{"verdict":"tight","minVramGb":5,"message":"Tight"}}]},
  "reason":null,"catalog":[]},
 {"repo":"org/Locked","url":"https://huggingface.co/org/Locked","downloads":5,"license":null,"pipelineTag":null,"gated":true,"likely":{"kind":"llm","label":"llm"},
  "preview":null,"reason":"Hugging Face requires a token for this repository.","reasonCode":"hub_auth_required","catalog":[]},
 {"repo":"Qwen/Qwen2.5-Coder-1.5B-Instruct-GGUF","url":"https://huggingface.co/Qwen/Qwen2.5-Coder-1.5B-Instruct-GGUF","downloads":9,"license":null,"pipelineTag":null,"gated":false,"likely":{"kind":"llm","label":"llm"},"preview":null,"reason":null,"catalog":[]}]}`

func discoverUI(t *testing.T, perms ...string) (*UI, *fakeDiscovery) {
	t.Helper()
	p := &auth.Principal{Kind: "browser-session", ID: "u", Permissions: perms}
	f := &fakeDiscovery{reply: map[string]string{"search": searchReply}, fail: map[string]string{}}
	cat, err := catalog.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return New(Deps{SignInEnabled: true, Principal: func(*http.Request) *auth.Principal { return p }, Call: f.call, Catalog: cat}), f
}

func discoverPost(u *UI, path string, form url.Values) string {
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("X-Localbase-Ui", "1")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	u.ServeHTTP(w, req)
	return w.Body.String()
}

// A search runs from the URL and renders cards: the TS recommendation is
// selected (never the low-bit file), files already in the catalog open the
// existing entry, a token-fixable reason links to Settings, and a qualified
// catalog model is offered first.
func TestDiscoverSearch(t *testing.T) {
	u, f := discoverUI(t, "models:read", "models:manage")
	body := get(u, "/app/models/get?q=+good+&kind=stt").Body.String()
	if got := f.sent[0]; got["action"] != "search" || got["query"] != "good" || got["kind"] != "stt" {
		t.Errorf("search sent %v", got)
	}
	for _, want := range []string{
		"3 results by downloads (from the local cache)", "12.3k downloads", `value="m-Q4_K_M.gguf" data-choice="hit-0-f0" data-fit="Fits"`,
		"Suggested download · Q4_K_M", "Low quality: prefer Q4 or a smaller model", "2 parts, total", "Already in catalog",
		`href="/app/models/m/good-q8">Open existing catalog entry`, "Accept terms on Hugging Face",
		"Hugging Face requires a token for this repository. <a", `href="/app/settings#hf-token"`,
		"Available in the Local catalog", `href="/app/models/m/qwen2.5-coder-1.5b-instruct-q4_k_m"`, "Download options",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, `value="m-IQ2_XS.gguf" data-choice="hit-0-f1" data-fit="Fits" checked`) || !strings.Contains(body, `value="m-Q4_K_M.gguf" data-choice="hit-0-f0" data-fit="Fits" data-fit-hint="Leaves room" checked`) {
		t.Error("the recommended file is not the one selected")
	}

	f.fail["search"] = `{"error":{"message":"Hugging Face rate-limited this search.","code":"hub_rate_limited"}}`
	if body := get(u, "/app/models/get?q=x").Body.String(); !strings.Contains(body, `role="alert">Hugging Face rate-limited this search. <a class="text-accent underline" href="/app/settings#hf-token">`) {
		t.Error("a rate-limited search does not offer the token")
	}
}

// Without models:manage the page explains, searches nothing, and the
// actions are refused.
func TestDiscoverReadOnly(t *testing.T) {
	u, f := discoverUI(t, "models:read")
	body := get(u, "/app/models/get?q=good").Body.String()
	if !strings.Contains(body, "Read-only access.") || strings.Contains(body, `role="search"`) || len(f.sent) != 0 {
		t.Errorf("read-only page: sent %v", f.sent)
	}
	if out := discoverPost(u, "/app/models/get/add", url.Values{"repo": {"org/x"}, "selector": {"a.gguf"}}); !strings.Contains(out, "Forbidden") && len(f.sent) != 0 {
		t.Errorf("read-only add: %s", out)
	}
}

// The card actions send TS's bodies and draw their results.
func TestDiscoverActions(t *testing.T) {
	u, f := discoverUI(t, "models:read", "models:manage")
	f.reply["preview"] = `{"preview":{"kind":"llm","defaultPath":null,"choices":[{"path":"only.gguf","sizeBytes":5,"partCount":1,"quant":null,"modelId":"only","existing":null,"fit":null}]}}`
	out := discoverPost(u, "/app/models/get/preview", url.Values{"repo": {"org/Other"}, "card": {"hit-4"}})
	if got := f.sent[0]; got["action"] != "preview" || got["repo"] != "org/Other" {
		t.Errorf("preview sent %v", got)
	}
	for _, want := range []string{`id="hit-4-files"`, "Selected download · Quantization unknown", "Memory fit is unknown.", `value="only.gguf" data-choice="hit-4-f0" data-fit="" data-fit-hint="" checked`} {
		if !strings.Contains(out, want) {
			t.Errorf("preview fragment: missing %q", want)
		}
	}
	if out := discoverPost(u, "/app/models/get/preview", url.Values{"repo": {"org/Other"}, "card": {`x" onclick="`}}); !strings.Contains(out, `id="card-files"`) {
		t.Error("an unsafe card id was not replaced")
	}

	f.reply["add"] = `{"modelId":"good-q4","displayName":"Good Model Q4_K_M","replaced":false,"operation":{"state":"running"}}`
	out = discoverPost(u, "/app/models/get/add", url.Values{"repo": {"org/Good-Model-GGUF"}, "selector": {"m-Q4_K_M.gguf"}, "install": {"1"}})
	if got := f.sent[len(f.sent)-1]; got["action"] != "add" || got["selector"] != "m-Q4_K_M.gguf" || got["install"] != true || got["repo"] != "org/Good-Model-GGUF" {
		t.Errorf("add sent %v", got)
	}
	if !strings.Contains(out, "Added Good Model Q4_K_M (good-q4); installing now.") || !strings.Contains(out, `href="/app/models/m/good-q4"`) {
		t.Errorf("add result: %s", out)
	}
	f.reply["add"] = `{"modelId":"good-q4","displayName":"Good Model Q4_K_M","replaced":true,"operation":{"state":"failed","detail":"disk full"}}`
	out = discoverPost(u, "/app/models/get/add", url.Values{"repo": {"org/Good-Model-GGUF"}, "selector": {"m-Q4_K_M.gguf"}, "install": {"1"}})
	if !strings.Contains(out, "Updated Good Model Q4_K_M (good-q4).") || !strings.Contains(out, "Added, but the install did not start: disk full") {
		t.Errorf("failed install: %s", out)
	}
	if out := discoverPost(u, "/app/models/get/add", url.Values{"repo": {"org/x"}}); !strings.Contains(out, "Choose a file to add.") {
		t.Errorf("no file: %s", out)
	}

	f.reply["install-recipe"] = `{"modelId":"rec","displayName":"Recipe Model","operation":{"state":"running"}}`
	out = discoverPost(u, "/app/models/get/recipe", url.Values{"modelId": {"rec"}})
	if got := f.sent[len(f.sent)-1]; got["action"] != "install-recipe" || got["modelId"] != "rec" || len(got) != 2 {
		t.Errorf("recipe sent %v", got)
	}
	if !strings.Contains(out, "Installing Recipe Model (rec).") {
		t.Errorf("recipe: %s", out)
	}
}
