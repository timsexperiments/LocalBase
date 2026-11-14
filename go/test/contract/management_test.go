package contract

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/test/harness"
)

// TestDifferentialModelManagement runs /_localbase/model-management: the catalog state, every
// rejection, enable/activate/disable, uninstall, and an install polled to completion. (Until step 7
// it compared each answer with TS's.)
func TestDifferentialModelManagement(t *testing.T) {
	tgs := pinnedGateways(t)
	const path = "/_localbase/model-management"
	post := func(key func(*harness.Target) string, body string) map[string]*harness.Resp {
		out := map[string]*harness.Resp{}
		for _, tg := range tgs {
			h := harness.Bearer(key(tg))
			h["Content-Type"] = "application/json"
			out[tg.Name] = harness.Do(t, "POST", tg.BaseURL+path, h, body)
		}
		return out
	}
	admin := func(tg *harness.Target) string { return tg.Admin }
	reader := func(tg *harness.Target) string { return tg.Key }
	state := func(tg *harness.Target) map[string]any {
		r := harness.Do(t, "GET", tg.BaseURL+path, harness.Bearer(tg.Admin), "")
		if r.Status != 200 || r.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s GET: %d %s", tg.Name, r.Status, r.Body)
		}
		return r.JSON(t)
	}
	// The catalog state at a step loads uncached (state's own check) and is pinned as Go answers it,
	// without the free space, which moves between reads.
	pinState := func(label string) {
		t.Helper()
		for _, tg := range tgs {
			pinAnswer(t, "state: "+label, harness.PruneJSON(state(tg), "", func(p string) bool { return strings.HasSuffix(p, ".availableBytes") }))
		}
	}
	wantStatus := func(label string, rs map[string]*harness.Resp, status int) {
		t.Helper()
		for name, r := range rs {
			if r.Status != status {
				t.Errorf("%s %s: %d %s", label, name, r.Status, r.Body)
			}
			var body any
			if json.Unmarshal(r.Body, &body) != nil {
				body = strings.TrimSpace(string(r.Body))
			}
			pinAnswer(t, "ok: "+label, map[string]any{"status": r.Status, "body": body})
		}
	}
	pinState("initial")
	// The active LLM, whichever earlier tests left as the default.
	activeLLM := harness.ChatModel
	for _, m := range state(tgs[0])["models"].([]any) {
		e := m.(map[string]any)
		if id, _ := e["id"].(string); e["active"] == true && slices.Contains([]string{harness.ChatModel, harness.DecisionModel, harness.EmbeddingModel}, id) {
			activeLLM = id
		}
	}
	for label, body := range sortedCases(map[string]string{
		"not json": `{`, "array": `[]`, "extra key": `{"modelId":"x","action":"enable","x":1}`, "empty id": `{"modelId":"","action":"enable"}`,
		"bad action": `{"modelId":"x","action":"delete"}`, "long id": `{"modelId":"` + strings.Repeat("m", 201) + `","action":"enable"}`,
		"too large": `{"modelId":"` + strings.Repeat("m", 1100) + `","action":"enable"}`, "unknown model": `{"modelId":"no-such","action":"enable"}`,
		"enable missing files": `{"modelId":"flux1-schnell-q4_0","action":"activate"}`,
		"uninstall selected":   `{"modelId":"whisper-base-en-q8_0","action":"uninstall"}`,
		"experimental":         `{"modelId":"no-such","action":"install"}`,
	}) {
		wantError(t, "management "+label, post(admin, body))
	}
	// Deliberate difference (DESIGN 13.6 item 10): Go's refusal names a way out.
	for name, r := range post(admin, `{"modelId":"`+activeLLM+`","action":"disable"}`) {
		if r.Status != 409 || r.ErrorCode(t) != "conflict" {
			t.Errorf("disable default llm %s: %d %s", name, r.Status, r.Body)
		}
	}
	wantError(t, "management as reader", post(reader, `{"modelId":"`+harness.TinyModel+`","action":"enable"}`))
	wantError(t, "management PUT", func() map[string]*harness.Resp {
		out := map[string]*harness.Resp{}
		for _, tg := range tgs {
			out[tg.Name] = harness.Do(t, "PUT", tg.BaseURL+path, harness.Bearer(tg.Admin), "")
		}
		return out
	}())

	// Install (already present: verified, then complete), polled to the end.
	wantStatus("install", post(admin, `{"modelId":"`+harness.TinyModel+`","action":"install"}`), 202)
	for _, tg := range tgs {
		deadline := time.Now().Add(30 * time.Second)
		for {
			op := harness.EntryOf(t, state(tg), harness.TinyModel)["operation"].(map[string]any)
			if op["state"] != "running" {
				if op["state"] != "complete" || op["detail"] != "Installation complete" {
					t.Errorf("%s install: %v", tg.Name, op)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s install still running: %v", tg.Name, op)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	pinState("after install")
	wantStatus("enable", post(admin, `{"modelId":"`+harness.TinyModel+`","action":"enable"}`), 200)
	wantError(t, "management install selected and installed", post(admin, `{"modelId":"`+harness.TinyModel+`","action":"install"}`))
	wantStatus("activate", post(admin, `{"modelId":"`+harness.TinyModel+`","action":"activate"}`), 200)
	pinState("after activate")
	wantStatus("disable active stt", post(admin, `{"modelId":"`+harness.TinyModel+`","action":"disable"}`), 200)
	// Go switches the active model on the next request and holds nothing, so it uninstalls (TS
	// still referenced the model from its running runtime and refused, 409).
	wantStatus("uninstall", post(admin, `{"modelId":"`+harness.TinyModel+`","action":"uninstall"}`), 200)
	pinState("after uninstall")
	for _, tg := range tgs {
		if e := harness.EntryOf(t, state(tg), harness.TinyModel); e["installed"] != false || e["installedBytes"] != 0.0 {
			t.Errorf("%s after uninstall: %v", tg.Name, e)
		}
	}
}
