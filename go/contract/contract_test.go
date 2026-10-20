package contract

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestHealth(t *testing.T) {
	each(t, func(t *testing.T, tg *Target) {
		r := do(t, "GET", tg.BaseURL+"/health", nil, "")
		if r.Status != 200 {
			t.Fatalf("status %d", r.Status)
		}
		body := r.JSON(t)
		if body["status"] != "ok" {
			t.Errorf("status = %v", body["status"])
		}
		mods, _ := body["modalities"].(map[string]any)
		for _, k := range []string{"llm", "stt", "tts", "image", "video"} {
			m, _ := mods[k].(map[string]any)
			if _, ok := m["configured"].(bool); !ok {
				t.Errorf("modalities.%s.configured missing: %s", k, r.Body)
			}
		}
		if _, ok := body["uptimeSeconds"].(float64); !ok {
			t.Error("uptimeSeconds missing")
		}
		if h := do(t, "HEAD", tg.BaseURL+"/health", nil, ""); h.Status != 200 || len(h.Body) != 0 {
			t.Errorf("HEAD = %d, %d bytes", h.Status, len(h.Body))
		}
		p := do(t, "POST", tg.BaseURL+"/health", nil, "")
		if p.Status != 405 || p.ErrorCode(t) != "method_not_allowed" || p.Header.Get("Allow") != "GET, HEAD" {
			t.Errorf("POST /health = %d %s allow=%q", p.Status, p.Body, p.Header.Get("Allow"))
		}
	})
}

func TestReadiness(t *testing.T) {
	each(t, func(t *testing.T, tg *Target) {
		r := do(t, "GET", tg.BaseURL+"/health/ready", nil, "")
		body := r.JSON(t)
		if r.Status != 200 || body["status"] != "ready" || body["reason"] != "request_admission_available" {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		mods, _ := body["modalities"].([]any)
		if len(mods) == 0 || mods[0] != "llm" {
			t.Errorf("modalities = %v", body["modalities"])
		}
	})
}

func TestAuthentication(t *testing.T) {
	each(t, func(t *testing.T, tg *Target) {
		r := do(t, "GET", tg.BaseURL+"/v1/models", nil, "")
		if r.Status != 401 || r.ErrorCode(t) != "invalid_api_key" || r.Header.Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("no key: %d %s", r.Status, r.Body)
		}
		if r := do(t, "GET", tg.BaseURL+"/v1/models", bearer("lb_not-a-real-key"), ""); r.Status != 401 {
			t.Errorf("bad key: %d", r.Status)
		}
		if r := do(t, "GET", tg.BaseURL+"/v1/models", map[string]string{"x-api-key": tg.Key}, ""); r.Status != 200 {
			t.Errorf("x-api-key: %d", r.Status)
		}
		// API keys never authenticate browser requests.
		h := bearer(tg.Key)
		h["Origin"] = "https://example.com"
		if r := do(t, "GET", tg.BaseURL+"/v1/models", h, ""); r.Status != 401 {
			t.Errorf("browser request with key: %d", r.Status)
		}
		if tg.ChatOnly != "" {
			r := do(t, "GET", tg.BaseURL+"/v1/models", bearer(tg.ChatOnly), "")
			if r.Status != 403 || r.ErrorCode(t) != "insufficient_permissions" {
				t.Errorf("chat-only key on models: %d %s", r.Status, r.Body)
			}
		}
	})
}

func TestUnmatchedRoute(t *testing.T) {
	each(t, func(t *testing.T, tg *Target) {
		if r := do(t, "GET", tg.BaseURL+"/v1/nope", nil, ""); r.Status != 401 {
			t.Errorf("unauthenticated unmatched: %d", r.Status)
		}
		r := do(t, "GET", tg.BaseURL+"/v1/nope/deeper", bearer(tg.Key), "")
		if r.Status != 404 || r.ErrorCode(t) != "route_disabled" {
			t.Errorf("unmatched: %d %s", r.Status, r.Body)
		}
	})
}

func models(t *testing.T, tg *Target) []map[string]any {
	r := do(t, "GET", tg.BaseURL+"/v1/models", bearer(tg.Key), "")
	if r.Status != 200 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	body := r.JSON(t)
	if body["object"] != "list" {
		t.Errorf("object = %v", body["object"])
	}
	var out []map[string]any
	for _, d := range body["data"].([]any) {
		out = append(out, d.(map[string]any))
	}
	return out
}

func TestModelsList(t *testing.T) {
	each(t, func(t *testing.T, tg *Target) {
		data := models(t, tg)
		ids := []string{}
		for _, m := range data {
			ids = append(ids, m["id"].(string))
			for _, k := range []string{"object", "created", "owned_by", "architecture"} {
				if _, ok := m[k]; !ok {
					t.Errorf("%s missing %s", m["id"], k)
				}
			}
			if m["object"] != "model" || m["owned_by"] != "local-base" {
				t.Errorf("%s: object/owned_by = %v/%v", m["id"], m["object"], m["owned_by"])
			}
		}
		want := []string{chatModel, decisionModel, embeddingModel}
		if !reflect.DeepEqual(ids, want) {
			t.Errorf("ids = %v, want %v", ids, want)
		}
		for _, m := range data {
			arch := m["architecture"].(map[string]any)
			out := arch["output_modalities"].([]any)
			switch m["id"] {
			case decisionModel:
				if out[0] != "decisions" {
					t.Errorf("decision output = %v", out)
				}
			case embeddingModel:
				if out[0] != "embeddings" {
					t.Errorf("embedding output = %v", out)
				}
			}
			if _, ok := m["context_budget"].(map[string]any); !ok {
				t.Errorf("%s has no context_budget", m["id"])
			}
		}
		// A chat-only key with models:read would list only chat models; the
		// chat-only fixture key lacks models:read, covered in TestAuthentication.
	})
}

func TestModelRetrieve(t *testing.T) {
	each(t, func(t *testing.T, tg *Target) {
		list := models(t, tg)
		for _, want := range list {
			id := want["id"].(string)
			r := do(t, "GET", tg.BaseURL+"/v1/models/"+id, bearer(tg.Key), "")
			if r.Status != 200 {
				t.Errorf("%s: %d %s", id, r.Status, r.Body)
				continue
			}
			got := r.JSON(t)
			if !sameIgnoring(got, want, "memory") {
				t.Errorf("%s retrieve differs from list entry:\n got %s\nwant %s", id, j(got), j(want))
			}
		}
		r := do(t, "GET", tg.BaseURL+"/v1/models/no%2Fsuch-model", bearer(tg.Key), "")
		if r.Status != 404 || r.ErrorCode(t) != "model_not_found" {
			t.Errorf("unknown: %d %s", r.Status, r.Body)
		}
		r = do(t, "POST", tg.BaseURL+"/v1/models/"+chatModel, bearer(tg.Key), "{}")
		if r.Status != 405 || r.Header.Get("Allow") != "GET" {
			t.Errorf("POST: %d allow=%q", r.Status, r.Header.Get("Allow"))
		}
	})
}

func TestChatRejections(t *testing.T) {
	each(t, func(t *testing.T, tg *Target) {
		post := func(body string) *Resp { return do(t, "POST", tg.BaseURL+"/v1/chat/completions", bearer(tg.Key), body) }
		r := post(`{"model":"` + decisionModel + `","messages":[{"role":"user","content":"hi"}]}`)
		if r.Status != 400 || r.ErrorCode(t) != "validation_failed" || !strings.Contains(string(r.Body), "/v1/systemone") {
			t.Errorf("decision model: %d %s", r.Status, r.Body)
		}
		r = post(`{"model":"` + embeddingModel + `","messages":[{"role":"user","content":"hi"}]}`)
		if r.Status != 400 || r.ErrorCode(t) != "validation_failed" {
			t.Errorf("embedding model: %d %s", r.Status, r.Body)
		}
		r = post(`{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}`)
		if r.Status != 404 || r.ErrorCode(t) != "model_not_found" {
			t.Errorf("unknown model: %d %s", r.Status, r.Body)
		}
		r = post(`{"model":"` + chatModel + `","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{}}}}]}`)
		if r.Status != 400 || r.ErrorCode(t) != "unsupported_model_capability" {
			t.Errorf("tools on non-tool model: %d %s", r.Status, r.Body)
		}
		if r := post(`{"messages":[]}`); r.Status != 400 {
			t.Errorf("missing model: %d %s", r.Status, r.Body)
		}
		// Deliberate difference: TS answers 400 (it parses the empty body as
		// JSON), Go answers 405. Both use the error envelope.
		if r := do(t, "GET", tg.BaseURL+"/v1/chat/completions", bearer(tg.Key), ""); r.Status != 400 && r.Status != 405 {
			t.Errorf("GET chat: %d", r.Status)
		} else {
			r.ErrorCode(t)
		}
	})
}

func TestChatCompletion(t *testing.T) {
	each(t, func(t *testing.T, tg *Target) {
		if !tg.RealModel {
			t.Skip("set LOCALBASE_CONTRACT_MODELS and LOCALBASE_CONTRACT_RUNTIMES for real inference")
		}
		r := do(t, "POST", tg.BaseURL+"/v1/chat/completions", bearer(tg.Key),
			`{"model":"`+chatModel+`","max_tokens":8,"messages":[{"role":"user","content":"Say hello."}]}`)
		if r.Status != 200 {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		body := r.JSON(t)
		choices, _ := body["choices"].([]any)
		if len(choices) == 0 {
			t.Fatalf("no choices: %s", r.Body)
		}
		msg := choices[0].(map[string]any)["message"].(map[string]any)
		if s, _ := msg["content"].(string); s == "" {
			t.Errorf("empty content: %s", r.Body)
		}
		if body["model"] != chatModel {
			t.Errorf("model = %v, want the public id %s", body["model"], chatModel)
		}
		if r.Header.Get("X-Localbase-Request-Id") == "" {
			t.Error("no x-localbase-request-id")
		}

		s := do(t, "POST", tg.BaseURL+"/v1/chat/completions", bearer(tg.Key),
			`{"model":"`+chatModel+`","stream":true,"max_tokens":8,"messages":[{"role":"user","content":"Count."}]}`)
		if s.Status != 200 || !strings.HasPrefix(s.Header.Get("Content-Type"), "text/event-stream") {
			t.Fatalf("stream: %d %s", s.Status, s.Header.Get("Content-Type"))
		}
		if !strings.Contains(string(s.Body), "data: [DONE]") {
			t.Errorf("stream did not end with [DONE]")
		}
		if strings.Contains(string(s.Body), ".gguf") || !strings.Contains(string(s.Body), `"model":"`+chatModel+`"`) {
			t.Errorf("stream chunks must carry the public model id")
		}
	})
}

// TestDifferentialModels compares the TS and Go /v1/models on identical
// fixtures. Only meaningful with -impl=both.
func TestDifferentialModels(t *testing.T) {
	if len(targets) < 2 {
		t.Skip("run with -impl=both")
	}
	a, b := models(t, targets[0]), models(t, targets[1])
	if len(a) != len(b) {
		t.Fatalf("%s has %d models, %s has %d", targets[0].Name, len(a), targets[1].Name, len(b))
	}
	// Measured peaks come from two different processes; compare presence only.
	volatile := func(m map[string]any) {
		if mem, ok := m["memory"].(map[string]any); ok && mem["observed_peak_bytes"] != nil {
			mem["observed_peak_bytes"] = "present"
		}
	}
	for i := range a {
		volatile(a[i])
		volatile(b[i])
		if !sameIgnoring(a[i], b[i]) {
			t.Errorf("%v differs:\n%s: %s\n%s: %s", a[i]["id"], targets[0].Name, j(a[i]), targets[1].Name, j(b[i]))
		}
	}
}

func sameIgnoring(a, b map[string]any, keys ...string) bool {
	strip := func(m map[string]any) map[string]any {
		c := map[string]any{}
		for k, v := range m {
			c[k] = v
		}
		for _, k := range keys {
			delete(c, k)
		}
		return c
	}
	return reflect.DeepEqual(strip(a), strip(b))
}

func j(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		b, _ := json.Marshal(v)
		return string(b)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b, _ := json.Marshal(m)
	return string(b)
}
