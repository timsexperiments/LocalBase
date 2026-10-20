package contract

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
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
		// Image models: the fixture selection, with the active one first (any
		// image request makes its model active).
		images := []string{"dreamshaper-8-lcm", "flux1-schnell-q4_0", "realvis-xl-v5.0", "sdxl-turbo"}
		if slices.Contains(ids, "dreamshaper-v8") {
			images = append(images, "dreamshaper-v8")
		}
		if first := slices.IndexFunc(ids, func(id string) bool { return slices.Contains(images, id) }); first >= 0 {
			if i := slices.Index(images, ids[first]); i > 0 {
				images = slices.Concat([]string{images[i]}, images[:i], images[i+1:])
			}
		}
		want := slices.Concat([]string{chatModel, decisionModel, embeddingModel, "whisper-base-en-q8_0", "qwen3-tts-1.7b-base-q4_k_m",
			"kokoro-82m-v1.0-q8"}, images, []string{"wan2.1-t2v-1.3b-q8_0", "fastwan2.2-ti2v-5b-q6_k"})
		// With a real whisper model linked, tiny.en is selected too, and a request
		// to it makes it the active (first) STT model.
		if slices.Contains(ids, "whisper-tiny-en-q8_0") {
			stt := []string{"whisper-base-en-q8_0", "whisper-tiny-en-q8_0"}
			if i := slices.Index(ids, "whisper-tiny-en-q8_0"); i < slices.Index(ids, "whisper-base-en-q8_0") {
				stt = []string{"whisper-tiny-en-q8_0", "whisper-base-en-q8_0"}
			}
			want = slices.Concat(want[:3], stt, want[4:])
		}
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
			_, hasBudget := m["context_budget"].(map[string]any)
			if isLLM := len(ids) > 0 && (m["id"] == chatModel || m["id"] == decisionModel || m["id"] == embeddingModel); hasBudget != isLLM {
				t.Errorf("%s: context_budget present = %v", m["id"], hasBudget)
			}
			if _, ok := m["memory"].(map[string]any); !ok {
				t.Errorf("%s has no memory figure", m["id"])
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

// rejectionCases fail before admission in both implementations.
var rejectionCases = []struct{ path, body string }{
	{"/v1/chat/completions", `not json`},
	{"/v1/chat/completions", `{"model":"","messages":[]}`},
	{"/v1/chat/completions", `{"model":"` + chatModel + `","messages":[{"role":"robot","content":"x"}]}`},
	{"/v1/chat/completions", `{"model":"` + chatModel + `","messages":[{"role":"assistant"}]}`},
	{"/v1/chat/completions", `{"model":"` + chatModel + `","messages":[],"temperature":3}`},
	{"/v1/chat/completions", `{"model":"` + chatModel + `","messages":[],"top_p":-1}`},
	{"/v1/chat/completions", `{"model":"` + chatModel + `","messages":[],"max_tokens":0}`},
	{"/v1/chat/completions", `{"model":"` + chatModel + `","messages":[{"role":"tool","content":"x"}]}`},
	{"/v1/chat/completions", `{"model":"` + chatModel + `","messages":[{"role":"user","content":[{"type":"image_url","image_url":{}}]}]}`},
	{"/v1/chat/completions", `{"model":"` + chatModel + `","messages":[],"tool_choice":{"type":"function","function":{"name":"x"}}}`},
	{"/v1/chat/completions", `{"model":"` + chatModel + `","messages":[],"tools":[{"type":"function","function":{"name":"f","extra":1}}]}`},
	{"/v1/chat/completions", `{"model":"` + chatModel + `","messages":[],"response_format":{"type":"yaml"}}`},
	{"/v1/chat/completions", `{"model":"` + chatModel + `","messages":[],"response_format":{"type":"json_schema","json_schema":{"name":"bad name","schema":{}}}}`},
	{"/v1/chat/completions", schemaCase(`{"type":"object","properties":{"a":{"type":"string","pattern":"x"}},"required":["a"],"additionalProperties":false}`)},
	{"/v1/chat/completions", schemaCase(`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`)},
	{"/v1/chat/completions", schemaCase(`{"type":"object","foo":1}`)},
	{"/v1/chat/completions", schemaCase(`{"type":"strng"}`)},
	{"/v1/chat/completions", schemaCase(`{"type":"object","properties":{},"required":["missing"],"additionalProperties":false}`)},
	{"/v1/chat/completions", schemaCase(`{"type":"array","items":{"type":"string"}}`)},
	{"/v1/chat/completions", schemaCase(`{"type":"object","properties":{"a":{"maxLength":3}},"required":["a"],"additionalProperties":false}`)},
	{"/v1/chat/completions", schemaCase(`{"type":"object","properties":{"a":{"type":"string","minLength":1}},"required":["a"],"additionalProperties":false}`)},
	{"/v1/chat/completions", schemaCase(`{"type":"object","properties":{"a":{"type":"number","minimum":1}},"required":["a"],"additionalProperties":false}`)},
	{"/v1/chat/completions", schemaCase(`{"type":"object","properties":{"a":{"$ref":"#/$defs/missing"}},"required":["a"],"additionalProperties":false}`)},
	{"/v1/chat/completions", schemaCase(`{"type":"object","properties":{"a":{"enum":["x",{"y":1}]}},"required":["a"],"additionalProperties":false}`)},
	{"/v1/chat/completions", schemaCase(`{"type":"object","properties":{"a":{"type":"string","format":"date"}},"required":["a"],"additionalProperties":false}`)},
	{"/v1/embeddings", `{"model":"` + embeddingModel + `"}`},
	{"/v1/embeddings", `{"model":"` + embeddingModel + `","input":[1,"a"]}`},
	{"/v1/embeddings", `{"model":"` + embeddingModel + `","input":"x","encoding_format":"base64"}`},
	{"/v1/embeddings", `{"model":"` + embeddingModel + `","input":"x","dimensions":-1}`},
	{"/v1/embeddings", `{"model":"` + embeddingModel + `","input":"x","dimensions":5000}`},
	{"/v1/embeddings", `{"model":"` + decisionModel + `","input":"x"}`},
	{"/v1/embeddings", `{"model":"no-such-model","input":"x"}`},
	{"/v1/systemone", `{"model":"` + decisionModel + `","state":null,"questions":{}}`},
	{"/v1/systemone", `{"model":"` + decisionModel + `","state":null,"questions":{"q":{"type":"choice","criteria":{"a":null}}}}`},
	{"/v1/systemone", `{"model":"` + decisionModel + `","state":null,"questions":{"q":{"type":"score","criteria":["a"]}}}`},
	{"/v1/systemone", `{"model":"` + decisionModel + `","state":null,"questions":{"q":{"type":"maybe"}}}`},
	{"/v1/systemone", `{"model":"` + decisionModel + `","state":null,"files":["x"],"questions":{"q":{"type":"noul"}}}`},
	{"/v1/systemone", `{"model":"` + decisionModel + `","state":null,"extra":1,"questions":{"q":{"type":"noul"}}}`},
	{"/v1/systemone", `{"model":"` + chatModel + `","state":null,"questions":{"q":{"type":"noul"}}}`},
	{"/v1/systemone", `{"model":"no-such-model","state":null,"questions":{"q":{"type":"noul"}}}`},
}

func schemaCase(schema string) string {
	return `{"model":"` + chatModel + `","messages":[{"role":"user","content":"x"}],"response_format":{"type":"json_schema","json_schema":{"name":"s","schema":` + schema + `}}}`
}

// TestDifferentialRejections sends requests that fail validation to every
// target and requires the same status, error type, code and param.
func TestDifferentialRejections(t *testing.T) {
	for _, c := range rejectionCases {
		var first *Resp
		var firstName string
		for _, tg := range targets {
			r := do(t, "POST", tg.BaseURL+c.path, bearer(tg.Key), c.body)
			if r.Status != 400 && r.Status != 404 {
				t.Errorf("%s %s %s: %d %s", tg.Name, c.path, c.body, r.Status, r.Body)
				continue
			}
			r.ErrorCode(t)
			if first == nil {
				first, firstName = r, tg.Name
				continue
			}
			ea, eb := first.JSON(t)["error"].(map[string]any), r.JSON(t)["error"].(map[string]any)
			for _, k := range []string{"type", "code", "param"} {
				if ea[k] != eb[k] || first.Status != r.Status {
					t.Errorf("%s %s:\n %s %d %s\n %s %d %s", c.path, c.body, firstName, first.Status, first.Body, tg.Name, r.Status, r.Body)
					break
				}
			}
		}
	}
}

func TestEmbeddings(t *testing.T) {
	each(t, func(t *testing.T, tg *Target) {
		if !tg.RealModel {
			t.Skip("needs LOCALBASE_CONTRACT_MODELS and LOCALBASE_CONTRACT_RUNTIMES")
		}
		r := do(t, "POST", tg.BaseURL+"/v1/embeddings", bearer(tg.Key), `{"model":"`+embeddingModel+`","input":["hello","world"]}`)
		if r.Status == 404 {
			t.Skip("embedding model file not linked")
		}
		if r.Status != 200 {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		body := r.JSON(t)
		data, _ := body["data"].([]any)
		if body["model"] != embeddingModel || len(data) != 2 {
			t.Fatalf("%s", r.Body)
		}
		vec, _ := data[0].(map[string]any)["embedding"].([]any)
		if len(vec) < 64 {
			t.Errorf("embedding has %d dimensions", len(vec))
		}
	})
}

// diffJSON returns the paths where a and b differ, skipping paths for which
// skip returns true.
func diffJSON(path string, a, b any, skip func(string) bool) []string {
	if skip != nil && skip(path) {
		return nil
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			return []string{path}
		}
		var out []string
		keys := map[string]bool{}
		for k := range av {
			keys[k] = true
		}
		for k := range bv {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			out = append(out, diffJSON(path+"."+k, av[k], bv[k], skip)...)
		}
		return out
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return []string{path + fmt.Sprintf(" (len %d vs %v)", len(av), len(bv))}
		}
		var out []string
		for i := range av {
			out = append(out, diffJSON(fmt.Sprintf("%s[%d]", path, i), av[i], bv[i], skip)...)
		}
		return out
	}
	if !reflect.DeepEqual(a, b) {
		return []string{fmt.Sprintf("%s: %v vs %v", path, a, b)}
	}
	return nil
}

// TestDifferentialModelMetadata compares /_localbase/models between the two
// implementations, apart from live host memory and runtime queue state.
func TestDifferentialModelMetadata(t *testing.T) {
	if len(targets) < 2 {
		t.Skip("run with -impl=both")
	}
	volatile := func(p string) bool {
		return p == ".host.memory.system.availableBytes" || strings.HasSuffix(p, ".device.runtime") ||
			strings.Contains(p, ".observedPeaks") || strings.Contains(p, ".memory.observed") ||
			strings.HasSuffix(p, ".memory.demandBytes") || strings.HasSuffix(p, ".memory.minimumVramEstimateGb") ||
			strings.HasSuffix(p, ".memory.source") || strings.HasSuffix(p, ".memory.launchShape") ||
			strings.HasSuffix(p, ".fit.headroomGb") || strings.HasSuffix(p, ".fit.verdict")
	}
	for _, path := range []string{"/_localbase/models", "/_localbase/models/" + chatModel, "/_localbase/models/" + embeddingModel,
		"/_localbase/models?kind=stt,tts&sort=size", "/_localbase/models?q=qwen%20coder", "/_localbase/models?capability=tool-calling&fit=fits,tight",
		"/_localbase/models?installed=true", "/_localbase/models?sort=context&reverse=true", "/_localbase/models?license=open&sort=name",
		"/_localbase/models?sort=fit", "/_localbase/models/no-such-model"} {
		var bodies []any
		var statuses []int
		for _, tg := range targets {
			r := do(t, "GET", tg.BaseURL+path, bearer(tg.Key), "")
			var v any
			json.Unmarshal(r.Body, &v)
			bodies, statuses = append(bodies, v), append(statuses, r.Status)
		}
		if statuses[0] != statuses[1] {
			t.Errorf("%s: status %d vs %d", path, statuses[0], statuses[1])
			continue
		}
		if d := diffJSON("", bodies[0], bodies[1], volatile); len(d) > 0 {
			if len(d) > 15 {
				d = append(d[:15], fmt.Sprintf("... %d more", len(d)-15))
			}
			t.Errorf("%s differs (%s vs %s):\n  %s", path, targets[0].Name, targets[1].Name, strings.Join(d, "\n  "))
		}
	}
}
