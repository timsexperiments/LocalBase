package contract

import (
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/test/harness"
)

// TestModelDiscoveryRejections: every rejection that needs no Hub request.
func TestModelDiscoveryRejections(t *testing.T) {
	tgs := pinnedGateways(t)
	const path = "/_localbase/model-discovery"
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
	for label, body := range sortedCases(map[string]string{
		"not json": `{`, "array": `[]`, "no action": `{}`, "unknown action": `{"action":"delete"}`,
		"search extra key": `{"action":"search","query":"x","limit":3}`, "search bad kind": `{"action":"search","kind":"tts"}`,
		"search long query":  `{"action":"search","query":"` + strings.Repeat("q", 201) + `"}`,
		"preview short repo": `{"action":"preview","repo":"ab"}`, "preview missing repo": `{"action":"preview"}`,
		"preview bad repo": `{"action":"preview","repo":"not a repo"}`, "add bad repo": `{"action":"add","repo":"a/../b"}`,
		"add empty selector": `{"action":"add","repo":"org/repo","selector":""}`, "add install string": `{"action":"add","repo":"org/repo","install":"yes"}`,
		"add owned id":         `{"action":"add","repo":"bad repo name","modelId":"` + harness.ChatModel + `"}`,
		"recipe unknown model": `{"action":"install-recipe","modelId":"no-such-model"}`,
		"recipe without feed":  `{"action":"install-recipe","modelId":"` + harness.ChatModel + `"}`,
		"too large":            `{"action":"search","query":"` + strings.Repeat("q", 2100) + `"}`,
	}) {
		wantError(t, "discovery "+label, post(admin, body))
	}
	wantError(t, "discovery as reader", post(func(tg *harness.Target) string { return tg.Key }, `{"action":"search"}`))
	get := map[string]*harness.Resp{}
	for _, tg := range tgs {
		get[tg.Name] = harness.Do(t, "GET", tg.BaseURL+path, harness.Bearer(tg.Admin), "")
	}
	wantError(t, "discovery GET", get)
}
