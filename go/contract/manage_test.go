package contract

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The admin APIs (/_localbase/api-keys, /_localbase/access-management,
// /_localbase/hf-token): every target runs the same script and must answer
// each step with the same status and, after masking ids, secrets and times,
// the same body. The script never reaches the network: email delivery points
// at a closed port and token saves skip the Hub check.

type manageStep struct {
	method, path, auth, body string
}

var (
	uuidRe   = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	timeRe   = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z`)
	secretRe = regexp.MustCompile(`lb_[A-Za-z0-9_-]+`)
)

const (
	keysPath   = "/_localbase/api-keys"
	accessPath = "/_localbase/access-management"
	hfPath     = "/_localbase/hf-token"
	origin     = `"origin":"https://lb.example.com","permissions":["inference:chat","models:read"]`
	policy     = `{"roles":[{"name":"chat","description":"Chat","permissions":["inference:chat"]},` +
		`{"name":"admin","description":"Admin","permissions":["access:read","access:manage","models:read"]}],` +
		`"bindings":[{"kind":"email","role":"admin","email":"owner@example.com"},` +
		`{"kind":"email-domain","role":"chat","domain":"example.com"}],"defaultRole":null}`
)

var manageScript = []manageStep{
	// api-keys
	{"GET", keysPath, "none", ""},
	{"GET", keysPath, "key", ""},
	{"GET", keysPath, "admin", ""},
	{"PUT", keysPath, "admin", `{}`},
	{"POST", keysPath, "key", `{"action":"create","name":"x","scopes":[]}`},
	{"POST", keysPath, "admin", `not json`},
	{"POST", keysPath, "admin", ``},
	{"POST", keysPath, "admin", `{"action":"nope"}`},
	{"POST", keysPath, "admin", `{"action":"create","name":"x"}`},
	{"POST", keysPath, "admin", `{"action":"create","name":"  ","scopes":[]}`},
	{"POST", keysPath, "admin", `{"action":"create","name":"x","scopes":["bogus"]}`},
	{"POST", keysPath, "admin", `{"action":"create","name":"x","scopes":[],"expiresDays":0}`},
	{"POST", keysPath, "admin", `{"action":"create","name":"x","scopes":[],"expiresDays":1.5}`},
	{"POST", keysPath, "admin", `{"action":"create","name":"x","scopes":[],"expiresDays":null}`},
	{"POST", keysPath, "admin", `{"action":"create","name":"x","scopes":[],"extra":1}`},
	{"POST", keysPath, "admin", `{"action":"rotate","keyId":"x","name":null}`},
	{"POST", keysPath, "admin", `{"action":"create","name":"x","scopes":[],"keyId":null}`},
	{"POST", keysPath, "admin", `{"action":"create","name":"made","scopes":["models:read","inference:chat","models:read"],"expiresDays":30}`},
	{"POST", keysPath, "admin", `{"action":"rotate","keyId":"missing"}`},
	{"POST", keysPath, "admin", `{"action":"rotate","keyId":""}`},
	{"POST", keysPath, "admin", `{"action":"rotate","keyId":"{{key}}"}`},
	{"POST", keysPath, "admin", `{"action":"set-scopes","keyId":"{{key}}","scopes":["keys:read"]}`},
	{"POST", keysPath, "admin", `{"action":"revoke","keyId":"{{key}}"}`},
	{"POST", keysPath, "admin", `{"action":"revoke","keyId":"{{key}}"}`},
	{"GET", keysPath, "admin", ""},
	// access-management
	{"GET", accessPath, "none", ""},
	{"GET", accessPath, "key", ""},
	{"GET", accessPath, "admin", ""},
	{"DELETE", accessPath, "admin", ""},
	{"POST", accessPath, "admin", `{"action":"test-policy","identity":{"issuer":"https://id.example.com","subject":"s"}}`},
	{"POST", accessPath, "admin", `{"action":"invite-user","email":"a@example.com","roles":["chat"]}`},
	{"POST", accessPath, "admin", `{"action":"apply-policy","policy":` + policy + `,"expectedPolicyRevision":null}`},
	{"POST", accessPath, "admin", `{"action":"test-email-delivery","to":"a@example.com"}`},
	{"POST", accessPath, "admin", `{"action":"upsert-magic-link","registration":{"kind":"magic-link","id":"email","name":"Email"},` + origin + `}`},
	{"POST", accessPath, "admin", `{"action":"configure-cloudflare","provider":{"kind":"cloudflare-access","teamDomain":"Bad","audience":"a"},` + origin + `}`},
	{"POST", accessPath, "admin", `{"action":"configure-cloudflare","provider":{"kind":"cloudflare-access","teamDomain":"team.cloudflareaccess.com","audience":"aud"},"origin":"http://lb.example.com","permissions":[]}`},
	{"POST", accessPath, "admin", `{"action":"configure-cloudflare","provider":{"kind":"cloudflare-access","teamDomain":"team.cloudflareaccess.com","audience":"aud"},` + origin + `}`},
	{"GET", accessPath, "admin", ""},
	{"POST", accessPath, "admin", `{"action":"upsert-oidc","registration":{"kind":"oidc","id":"google","name":"Google","issuer":"https://accounts.google.com","clientId":"cid","clientAuthentication":{"kind":"client-secret-basic","clientSecret":"shh"}},` + origin + `}`},
	{"POST", accessPath, "admin", `{"action":"configure-email-delivery","config":{"host":"127.0.0.1","port":1,"security":"starttls","authentication":{"kind":"none"},"from":"LocalBase@example.com"}}`},
	{"POST", accessPath, "admin", `{"action":"configure-email-delivery","config":{"host":"127.0.0.1","port":0,"security":"starttls","authentication":{"kind":"none"},"from":"lb@example.com"}}`},
	{"POST", accessPath, "admin", `{"action":"upsert-magic-link","registration":{"kind":"magic-link","id":"email","name":"Email"},` + origin + `}`},
	{"POST", accessPath, "admin", `{"action":"upsert-magic-link","registration":{"kind":"magic-link","id":"email2","name":"Email 2"},` + origin + `}`},
	{"POST", accessPath, "admin", `{"action":"disable-email-delivery"}`},
	{"POST", accessPath, "admin", `{"action":"test-email-delivery","to":"a@example.com"}`},
	{"GET", accessPath, "admin", ""},
	{"POST", accessPath, "admin", `{"action":"test-policy","identity":{"issuer":"https://accounts.google.com","subject":"s","verifiedEmail":"owner@example.com"}}`},
	{"POST", accessPath, "admin", `{"action":"apply-policy","policy":` + policy + `,"expectedPolicyRevision":"` + strings.Repeat("0", 64) + `"}`},
	{"POST", accessPath, "admin", `{"action":"apply-policy","policy":{"roles":[],"bindings":[],"defaultRole":"x"},"expectedPolicyRevision":null}`},
	{"POST", accessPath, "admin", `{"action":"apply-policy","policy":` + policy + `,"expectedPolicyRevision":null}`},
	{"GET", accessPath, "admin", ""},
	{"POST", accessPath, "admin", `{"action":"test-policy","identity":{"issuer":"https://accounts.google.com","subject":"s","verifiedEmail":"owner@example.com"}}`},
	{"POST", accessPath, "admin", `{"action":"test-policy","identity":{"issuer":"https://accounts.google.com","subject":"s","verifiedEmail":"x@example.com"}}`},
	{"POST", accessPath, "admin", `{"action":"test-policy","identity":{"issuer":"https://accounts.google.com","subject":"s"}}`},
	{"POST", accessPath, "admin", `{"action":"invite-user","email":"Friend@Example.com","roles":["chat"]}`},
	{"POST", accessPath, "admin", `{"action":"invite-user","email":"friend@example.com","roles":["chat"]}`},
	{"POST", accessPath, "admin", `{"action":"invite-user","email":"other@example.com","roles":["missing"]}`},
	{"POST", accessPath, "admin", `{"action":"invite-user","email":"other@example.com","roles":[]}`},
	{"POST", accessPath, "admin", `{"action":"replace-user-roles","email":"friend@example.com","roles":["chat","admin"]}`},
	{"POST", accessPath, "admin", `{"action":"disable-user","email":"friend@example.com"}`},
	{"POST", accessPath, "admin", `{"action":"enable-user","email":"friend@example.com"}`},
	{"POST", accessPath, "admin", `{"action":"enable-user","email":"nobody@example.com"}`},
	{"GET", accessPath, "admin", ""},
	{"POST", accessPath, "admin", `{"action":"clear-policy","expectedPolicyRevision":"{{rev}}"}`},
	{"POST", accessPath, "admin", `{"action":"remove-user","email":"friend@example.com"}`},
	{"POST", accessPath, "admin", `{"action":"clear-policy","expectedPolicyRevision":"{{rev}}"}`},
	{"POST", accessPath, "admin", `{"action":"remove-registration","registrationId":"missing"}`},
	{"POST", accessPath, "admin", `{"action":"remove-registration","registrationId":"email"}`},
	{"POST", accessPath, "admin", `{"action":"disable-email-delivery"}`},
	{"POST", accessPath, "admin", `{"action":"remove-registration","registrationId":"google"}`},
	{"POST", accessPath, "admin", `{"action":"disable"}`},
	{"POST", accessPath, "admin", `{"action":"disable","extra":true}`},
	{"POST", accessPath, "admin", `{"action":"invite-user","email":"n@example.com","roles":["chat"],"sendEmail":null}`},
	{"POST", accessPath, "admin", `{"action":"clear-policy","expectedPolicyRevision":null}`},
	{"GET", accessPath, "admin", ""},
	// hf-token
	{"GET", hfPath, "none", ""},
	{"PUT", hfPath, "key", ""},
	{"GET", hfPath, "key", ""},
	{"GET", hfPath, "admin", ""},
	{"POST", hfPath, "admin", `not json`},
	{"POST", hfPath, "admin", `{"action":"save","token":"has space inside"}`},
	{"POST", hfPath, "admin", `{"action":"save","token":"short"}`},
	{"POST", hfPath, "admin", `{"action":"save","token":"hf_abcdefgh","validate":null}`},
	{"POST", hfPath, "admin", `{"action":"clear","token":"hf_abcdefgh"}`},
	{"POST", hfPath, "admin", `{"action":"validate"}`},
	{"POST", hfPath, "admin", `{"action":"save","token":"  hf_abcdefghijkl  ","validate":false}`},
	{"GET", hfPath, "admin", ""},
	{"POST", hfPath, "admin", `{"action":"clear"}`},
	{"POST", hfPath, "admin", `{"action":"save","token":"` + strings.Repeat("a", 1100) + `"}`},
}

type manageResult struct {
	status int
	body   any
	raw    string
}

func runManageScript(t *testing.T, tg *Target) []manageResult {
	var key, rev string
	var out []manageResult
	for _, s := range manageScript {
		h := map[string]string{}
		switch s.auth {
		case "key":
			h = bearer(tg.Key)
		case "admin":
			h = bearer(tg.Admin)
		}
		body := strings.NewReplacer("{{key}}", key, "{{rev}}", rev).Replace(s.body)
		r := do(t, s.method, tg.BaseURL+s.path, h, body)
		if r.Header.Get("Cache-Control") != "no-store" && r.Status != 405 && !(s.path == hfPath && r.Status < 404 && r.Status > 400) {
			t.Errorf("%s %s %s: Cache-Control %q", s.method, s.path, s.body, r.Header.Get("Cache-Control"))
		}
		var parsed any
		if len(r.Body) > 0 {
			if err := json.Unmarshal(r.Body, &parsed); err != nil {
				parsed = string(r.Body)
			}
		}
		if m, ok := parsed.(map[string]any); ok {
			if k, ok := m["key"].(map[string]any); ok && strings.Contains(s.body, `"create"`) {
				key, _ = k["id"].(string)
			}
			if v, ok := m["policyRevision"].(string); ok {
				rev = v
			}
		}
		masked := uuidRe.ReplaceAllString(string(r.Body), "<uuid>")
		masked = timeRe.ReplaceAllString(masked, "<time>")
		masked = secretRe.ReplaceAllString(masked, "<secret>")
		var norm any
		if json.Unmarshal([]byte(masked), &norm) != nil {
			norm = masked
		}
		maskKeys(norm)
		out = append(out, manageResult{r.Status, norm, string(r.Body)})
	}
	return out
}

func TestDifferentialManagement(t *testing.T) {
	results := map[string][]manageResult{}
	for _, tg := range targets {
		if tg.Admin == "" {
			t.Skip("needs launched gateways")
		}
		results[tg.Name] = runManageScript(t, tg)
	}
	if len(targets) < 2 {
		return
	}
	a, b := results[targets[0].Name], results[targets[1].Name]
	for i, s := range manageScript {
		ra, rb := a[i], b[i]
		diffs := diffJSON("", ra.body, rb.body, nil)
		if ra.status != rb.status || len(diffs) > 0 {
			t.Errorf("step %d %s %s %s\n  %s %d %s\n  %s %d %s\n  %v", i, s.method, s.path, s.body,
				targets[0].Name, ra.status, ra.raw, targets[1].Name, rb.status, rb.raw, diffs)
		}
	}
}

// maskKeys hides key prefixes and orders key lists by name: fixture keys
// share a creation millisecond on one side only, which decides their order.
func maskKeys(v any) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	mask := func(k any) {
		if km, ok := k.(map[string]any); ok {
			km["prefix"] = "<prefix>"
		}
	}
	mask(m["key"])
	if keys, ok := m["keys"].([]any); ok {
		for _, k := range keys {
			mask(k)
		}
		sort.SliceStable(keys, func(i, j int) bool {
			return keys[i].(map[string]any)["name"].(string) < keys[j].(map[string]any)["name"].(string)
		})
	}
}
