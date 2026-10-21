package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

// The default chat models API (DESIGN §18).
func TestDefaultChatModelsAPI(t *testing.T) {
	h := &steppedHost{}
	h.avail.Store(50 * memory.GiB)
	ts, llm, _ := newTestGatewayWith(t, h, "qwen3.5-4b-q4_k_m")
	get := func() (int, map[string]any) {
		resp, err := http.Get(ts.URL + "/_localbase/default-chat-models")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		b, _ := io.ReadAll(resp.Body)
		json.Unmarshal(b, &m)
		return resp.StatusCode, m
	}
	if code, m := get(); code != 200 || m["models"].([]any)[0] != chatModel {
		t.Fatalf("get: %d %v", code, m)
	}
	for _, bad := range []string{`{}`, `{"models":[]}`, `{"models":["` + decisionModel + `"]}`, `{"models":["nope"]}`,
		`{"models":["` + chatModel + `","` + chatModel + `"]}`, `{"models":["` + embeddingModel + `"]}`} {
		if r := post(t, ts, "/_localbase/default-chat-models", bad); r.status != 400 {
			t.Errorf("%s: %d %s", bad, r.status, r.body)
		}
	}
	// Two chat models: a list. The first becomes the active model.
	r := post(t, ts, "/_localbase/default-chat-models", `{"models":["qwen3.5-4b-q4_k_m","`+chatModel+`"]}`)
	if r.status != 200 || !strings.Contains(r.body, `"preferredModelId":"qwen3.5-4b-q4_k_m"`) {
		t.Fatalf("post: %d %s", r.status, r.body)
	}
	// A list leaves activeLlmModel alone (a rollback to TS launches it).
	got := llm.Config()
	if len(got.DefaultLLMs) != 2 || got.DefaultLLMs[0] != "qwen3.5-4b-q4_k_m" || got.Active["llm"] != chatModel {
		t.Errorf("config after post: defaults %v active %s", got.DefaultLLMs, got.Active["llm"])
	}
	// One model: the active model alone, no list.
	if r := post(t, ts, "/_localbase/default-chat-models", `{"models":["`+chatModel+`"]}`); r.status != 200 {
		t.Fatalf("post one: %d %s", r.status, r.body)
	}
	if got := llm.Config(); got.DefaultLLMs != nil || got.Active["llm"] != chatModel {
		t.Errorf("one default: list %v active %s", got.DefaultLLMs, got.Active["llm"])
	}
}

// The CLI's status endpoint needs the instance token.
func TestDefaultModelNeedsInstanceToken(t *testing.T) {
	h := &steppedHost{}
	h.avail.Store(50 * memory.GiB)
	ts, _, _ := newTestGateway(t, h)
	resp, err := http.Get(ts.URL + "/_localbase/default-model")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("without a token: %d", resp.StatusCode)
	}
}
