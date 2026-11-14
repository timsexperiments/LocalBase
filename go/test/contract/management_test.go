package contract

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/test/harness"
)

// TestDifferentialModelManagement compares /_localbase/model-management on
// both gateways: the catalog state, every rejection, enable/activate/disable,
// uninstall, and an install polled to completion.
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
	same := func(label string) {
		t.Helper()
		var first map[string]any
		for _, tg := range tgs {
			s := state(tg)
			// Go's state as it is, before the differences from TS are evened out below; only the free
			// space, which moves between reads, is left out.
			pinAnswer(t, "state: "+label, tg.Name, harness.PruneJSON(s, "", func(p string) bool { return strings.HasSuffix(p, ".availableBytes") }))
			// Deliberate difference (DESIGN 13.6 item 10): Go can install a
			// configured model with files missing; TS refuses. Only that.
			for _, m := range s["models"].([]any) {
				e := m.(map[string]any)
				if e["installed"] == false && (e["enabled"] == true || e["active"] == true) {
					delete(e, "canInstall")
					delete(e, "installUnavailableReason")
				}
				// Both refuse; a configured model with files missing no longer
				// counts on Go, so which model the sentence names can differ.
				if r, _ := e["installUnavailableReason"].(string); strings.HasPrefix(r, "Disable ") {
					e["installUnavailableReason"] = "Disable"
				}
				// Deliberate difference (DESIGN's list of them): TS names a Linux target that doesn't exist. Only
				// these exact sentences; any other reason must still match.
				if r, _ := e["installUnavailableReason"].(string); slices.Contains([]string{"Requires Linux x64 with a single NVIDIA GPU.",
					"Video generation runs only on Apple Silicon Macs today.",
					"This video model can't run on any computer LocalBase supports today. Choose another video model."}, r) {
					e["installUnavailableReason"] = "video refusal"
				}
			}
			if first == nil {
				first = s
				continue
			}
			// Free space moves between two reads; everything else must match.
			if d := harness.DiffJSON("", first, s, func(p string) bool { return strings.HasSuffix(p, ".availableBytes") }); len(d) > 0 {
				if len(d) > 15 {
					d = d[:15]
				}
				t.Errorf("%s: state differs:\n  %s", label, strings.Join(d, "\n  "))
			}
		}
	}
	sameOK := func(label string, rs map[string]*harness.Resp, status int) {
		t.Helper()
		var first string
		for name, r := range rs {
			if r.Status != status {
				t.Errorf("%s %s: %d %s", label, name, r.Status, r.Body)
			}
			body := strings.TrimSpace(string(r.Body))
			var parsed any
			if json.Unmarshal(r.Body, &parsed) != nil {
				parsed = body
			}
			pinAnswer(t, "ok: "+label, name, map[string]any{"status": r.Status, "body": parsed})
			if first == "" {
				first = body
			} else if body != first {
				t.Errorf("%s: bodies differ:\n %s\n %s", label, first, r.Body)
			}
		}
	}
	// Both start from the fixture's chat model: the test's own gateways have taken no earlier
	// decision request, which would make the decision model active on TS (DESIGN §13.6 item 9).
	same("initial")
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
		sameError(t, "management "+label, post(admin, body))
	}
	// Deliberate difference (DESIGN 13.6 item 10): Go's refusal names a way out.
	for name, r := range post(admin, `{"modelId":"`+activeLLM+`","action":"disable"}`) {
		if r.Status != 409 || r.ErrorCode(t) != "conflict" {
			t.Errorf("disable default llm %s: %d %s", name, r.Status, r.Body)
		}
	}
	sameError(t, "management as reader", post(reader, `{"modelId":"`+harness.TinyModel+`","action":"enable"}`))
	sameError(t, "management PUT", func() map[string]*harness.Resp {
		out := map[string]*harness.Resp{}
		for _, tg := range tgs {
			out[tg.Name] = harness.Do(t, "PUT", tg.BaseURL+path, harness.Bearer(tg.Admin), "")
		}
		return out
	}())

	// Install (already present: verified, then complete), polled to the end.
	sameOK("install", post(admin, `{"modelId":"`+harness.TinyModel+`","action":"install"}`), 202)
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
	same("after install")
	sameOK("enable", post(admin, `{"modelId":"`+harness.TinyModel+`","action":"enable"}`), 200)
	sameError(t, "management install selected and installed", post(admin, `{"modelId":"`+harness.TinyModel+`","action":"install"}`))
	sameOK("activate", post(admin, `{"modelId":"`+harness.TinyModel+`","action":"activate"}`), 200)
	same("after activate")
	sameOK("disable active stt", post(admin, `{"modelId":"`+harness.TinyModel+`","action":"disable"}`), 200)
	// Deliberate difference: TS reconciles a running runtime eagerly when the
	// active model changes, so while its STT runtime is up it still references
	// the model and refuses the uninstall (409 conflict). Go switches on the
	// next request, holds nothing, and uninstalls.
	tsEager := false
	for _, tg := range tgs {
		if tg.Name == "ts" {
			mods := harness.Do(t, "GET", tg.BaseURL+"/health", nil, "").JSON(t)["modalities"].(map[string]any)
			state := mods["stt"].(map[string]any)["state"]
			tsEager = state != "idle" && state != "disabled"
		}
	}
	uninstall := post(admin, `{"modelId":"`+harness.TinyModel+`","action":"uninstall"}`)
	if !tsEager {
		sameOK("uninstall", uninstall, 200)
		same("after uninstall")
	} else if r := uninstall["ts"]; r.Status != 409 || r.ErrorCode(t) != "conflict" {
		t.Errorf("ts uninstall with a running STT runtime: %d %s", r.Status, r.Body)
	}
	for _, tg := range tgs {
		if tg.Name == "ts" && tsEager {
			continue
		}
		if e := harness.EntryOf(t, state(tg), harness.TinyModel); e["installed"] != false || e["installedBytes"] != 0.0 {
			t.Errorf("%s after uninstall: %v", tg.Name, e)
		}
	}
}
