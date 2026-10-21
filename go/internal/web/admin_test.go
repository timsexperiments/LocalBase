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
)

// fakeAdmin answers the two admin APIs like the gateway, enforcing the
// read/manage permissions, and records what the page sent.
type fakeAdmin struct {
	access string // GET body
	keys   string
	sent   []map[string]any
	reply  map[string]string // action -> success body
	fail   map[string]string // action -> error message (409)
}

func (f *fakeAdmin) call(_ context.Context, p *auth.Principal, method, path, _ string, body []byte) APIResult {
	area := map[string]string{"/_localbase/access-management": "access", "/_localbase/api-keys": "keys"}[path]
	if area == "" || !p.Can(area+":read") {
		return APIResult{Status: 403, Body: []byte(`{"error":{"message":"forbidden"}}`)}
	}
	if method == "GET" {
		if area == "access" {
			return APIResult{Status: 200, Body: []byte(f.access)}
		}
		return APIResult{Status: 200, Body: []byte(f.keys)}
	}
	if !p.Can(area + ":manage") {
		return APIResult{Status: 403, Body: []byte(`{"error":{"message":"forbidden"}}`)}
	}
	var in map[string]any
	json.Unmarshal(body, &in)
	f.sent = append(f.sent, in)
	action, _ := in["action"].(string)
	if msg, ok := f.fail[action]; ok {
		return APIResult{Status: 409, Body: []byte(`{"error":{"message":"` + msg + `"}}`)}
	}
	if b, ok := f.reply[action]; ok {
		return APIResult{Status: 200, Body: []byte(b)}
	}
	return APIResult{Status: 200, Body: []byte(`{}`)}
}

const adminAccess = `{"config":{"origin":"https://lb.example.com","permissions":["inference:chat","models:read"],"provider":{"kind":"direct","registrations":[{"kind":"oidc","id":"google","name":"Google","issuer":"https://accounts.google.com","clientId":"cid","clientAuthentication":"client-secret-basic"}]}},
 "emailDelivery":null,
 "policy":{"roles":[{"name":"admin","description":"Admins","permissions":["access:read","access:manage"]},{"name":"chat","description":"","permissions":["inference:chat"]}],"bindings":[{"kind":"email","role":"admin","email":"owner@example.com"}],"defaultRole":"chat"},
 "policyRevision":"abc123",
 "roles":[{"name":"admin","description":"Admins","permissions":["access:read","access:manage"]},{"name":"chat","description":"","permissions":["inference:chat"]}],
 "users":[{"id":"u1","email":"owner@example.com","status":"active","roles":["admin"],"createdAt":"2026-10-01T00:00:00.000Z","updatedAt":"2026-10-01T00:00:00.000Z"},
          {"id":"u2","email":"friend@example.com","status":"disabled","roles":["chat"],"createdAt":"2026-10-02T00:00:00.000Z","updatedAt":"2026-10-02T00:00:00.000Z"}]}`

const adminKeys = `{"keys":[{"id":"key_1","name":"laptop","prefix":"AbCd1234","scopes":["inference:chat"],"createdAt":"2026-10-01T00:00:00.000Z","lastRotatedAt":"2026-10-01T00:00:00.000Z"},
 {"id":"key_2","name":"old","prefix":"ZzZz9999","scopes":["models:read"],"createdAt":"2026-01-01T00:00:00.000Z","lastRotatedAt":"2026-01-01T00:00:00.000Z","revokedAt":"2026-02-01T00:00:00.000Z"}]}`

func adminUI(perms ...string) (*UI, *fakeAdmin, *auth.Principal) {
	p := &auth.Principal{Kind: "browser-session", ID: "browser:owner-hash", Name: "owner@example.com", Permissions: perms,
		Identity: &auth.Identity{Issuer: "https://accounts.google.com", Subject: "s", VerifiedEmail: "owner@example.com"}}
	f := &fakeAdmin{access: adminAccess, keys: adminKeys, reply: map[string]string{}, fail: map[string]string{}}
	return New(Deps{SignInEnabled: true, Principal: func(*http.Request) *auth.Principal { return p }, Call: f.call}), f, p
}

func adminPost(u *UI, path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("X-Localbase-Ui", "1")
	withCSRF(u, req)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	u.ServeHTTP(w, req)
	return w
}

// Admin opens for access:read or keys:read, with only the sections those
// allow, and the header links to it only then.
func TestAdminSections(t *testing.T) {
	u, _, p := adminUI("models:read")
	if w := get(u, "/app/admin"); w.Code != http.StatusForbidden {
		t.Errorf("without read permissions: %d", w.Code)
	}
	if strings.Contains(get(u, "/app/settings").Body.String(), `href="/app/admin"`) {
		t.Error("the header links to Admin without permission")
	}
	p.Permissions = []string{"models:read", "keys:read"}
	body := get(u, "/app/admin?section=people").Body.String()
	if !strings.Contains(body, `aria-current="page">API keys`) || strings.Contains(body, "People &amp; roles") || !strings.Contains(body, "laptop") {
		t.Error("keys:read alone should land on API keys only")
	}
	if strings.Contains(body, "Create API key") || strings.Contains(body, "Rotate") || !strings.Contains(body, "inference:chat</p>") {
		t.Error("keys:read sees management controls, or not the scopes")
	}
	if !strings.Contains(body, `<span class="badge badge-ok">Active</span>`) || !strings.Contains(body, `<span class="badge ">Revoked</span>`) {
		t.Error("key statuses")
	}
}

// Read-only access shows lists and summaries, no forms.
func TestAdminReadOnlyAccess(t *testing.T) {
	u, _, _ := adminUI("models:read", "access:read")
	people := get(u, "/app/admin?section=people").Body.String()
	for _, want := range []string{"friend@example.com", "disabled · chat", "Default: chat", "access:read, access:manage"} {
		if !strings.Contains(people, want) {
			t.Errorf("read-only people: missing %q", want)
		}
	}
	access := get(u, "/app/admin?section=access").Body.String()
	if !strings.Contains(access, "Direct sign-in · 1 registration") || !strings.Contains(access, "2 roles configured") || strings.Contains(access, "hx-post") {
		t.Error("read-only access view")
	}
}

// Managers get the forms; the page sends TS's bodies, confirms destructive
// actions, and marks the signed-in person's own row.
func TestAdminPeopleActions(t *testing.T) {
	u, f, _ := adminUI("models:read", "access:read", "access:manage")
	body := get(u, "/app/admin?section=people").Body.String()
	for _, want := range []string{"Invite a person", `value="chat" checked`, "owner@example.com <span class=\"text-sm font-normal text-muted\">(you)</span>",
		"You can’t disable or forget your own account", "Confirm forget", `<option value="chat" selected>chat</option>`, "Enable access"} {
		if !strings.Contains(body, want) {
			t.Errorf("people: missing %q", want)
		}
	}
	if strings.Count(body, ">Forget record</summary>") != 1 {
		t.Error("the signed-in person can forget their own record")
	}

	f.reply["invite-user"] = `{"signInUrl":"https://lb.example.com/app/login","emailDelivered":false,"user":{}}`
	out := adminPost(u, "/app/admin/access", url.Values{"section": {"people"}, "action": {"invite-user"}, "email": {"new@example.com"}, "roles": {"chat"}, "sendEmail": {"1"}}).Body.String()
	if got := f.sent[0]; got["email"] != "new@example.com" || got["sendEmail"] != false {
		t.Errorf("invite sent %v (no SMTP, so no email)", got)
	}
	if !strings.Contains(out, "User invited. Copy the sign-in link.") || !strings.Contains(out, "https://lb.example.com/app/login</code>") {
		t.Errorf("invite result: %s", out)
	}
	if strings.Contains(get(u, "/app/admin?section=people").Body.String(), "Invitation link") {
		t.Error("the invitation link is shown again")
	}

	adminPost(u, "/app/admin/access", url.Values{"section": {"people"}, "action": {"replace-user-roles"}, "email": {"friend@example.com"}})
	if got := f.sent[1]; got["action"] != "replace-user-roles" || len(got["roles"].([]any)) != 0 {
		t.Errorf("clearing roles sent %v", got)
	}
	f.fail["disable-user"] = "You can't disable or forget your own account."
	if out := adminPost(u, "/app/admin/access", url.Values{"section": {"people"}, "action": {"disable-user"}, "email": {"owner@example.com"}}).Body.String(); !strings.Contains(out, `role="alert">You can&#39;t disable`) {
		t.Errorf("the API's lockout refusal is not shown: %s", out)
	}

	adminPost(u, "/app/admin/access", url.Values{"section": {"people"}, "action": {"create-role"}, "name": {"viewer"}, "description": {"Looks"}, "permissions": {"models:read", "bogus"}})
	got := f.sent[len(f.sent)-1]
	policy := got["policy"].(map[string]any)
	roles := policy["roles"].([]any)
	if got["action"] != "apply-policy" || got["expectedPolicyRevision"] != "abc123" || len(roles) != 3 || roles[2].(map[string]any)["permissions"].([]any)[0] != "models:read" || len(roles[2].(map[string]any)["permissions"].([]any)) != 1 {
		t.Errorf("create role sent %v", got)
	}
	adminPost(u, "/app/admin/access", url.Values{"section": {"people"}, "action": {"default-role"}, "name": {""}})
	if got := f.sent[len(f.sent)-1]; got["policy"].(map[string]any)["defaultRole"] != nil {
		t.Errorf("clearing the default role sent %v", got)
	}
}

// Provider and policy changes, with secrets never pre-filled.
func TestAdminAccessActions(t *testing.T) {
	u, f, _ := adminUI("models:read", "access:read", "access:manage")
	body := get(u, "/app/admin?section=access&edit=google").Body.String()
	for _, want := range []string{`value="google" placeholder="google" readonly`, `value="https://accounts.google.com"`, `name="clientSecret" required autocomplete="new-password" placeholder="Re-enter to save">`,
		"Removing the last one disables browser access.", `defaultRole&#34;: &#34;chat`, `name="revision" value="abc123"`, "Confirm clear"} {
		if !strings.Contains(body, want) {
			t.Errorf("access: missing %q", want)
		}
	}
	adminPost(u, "/app/admin/access", url.Values{"section": {"access"}, "action": {"save-provider"}, "provider": {"oidc"}, "origin": {"https://lb.example.com"},
		"registrationId": {"google"}, "registrationName": {"Google"}, "issuer": {"https://accounts.google.com"}, "clientId": {"cid"}, "publicClient": {"1"}, "permissions": {"models:read", "inference:chat"}})
	got, _ := json.Marshal(f.sent[0])
	if !strings.Contains(string(got), `"action":"upsert-oidc"`) || !strings.Contains(string(got), `"clientAuthentication":{"kind":"none"}`) || !strings.Contains(string(got), `"permissions":["inference:chat","models:read"]`) {
		t.Errorf("provider sent %s", got)
	}
	if out := adminPost(u, "/app/admin/access", url.Values{"section": {"access"}, "action": {"apply-policy"}, "policy": {"{nope"}}).Body.String(); !strings.Contains(out, "The policy is not valid JSON.") {
		t.Error("invalid policy JSON")
	}
	adminPost(u, "/app/admin/access", url.Values{"section": {"access"}, "action": {"clear-policy"}, "revision": {"abc123"}})
	if got := f.sent[len(f.sent)-1]; got["action"] != "clear-policy" || got["expectedPolicyRevision"] != "abc123" {
		t.Errorf("clear sent %v", got)
	}
}

// A new or rotated secret appears once, in the change's response.
func TestAdminKeySecretOnce(t *testing.T) {
	u, f, _ := adminUI("models:read", "keys:read", "keys:manage")
	f.reply["create"] = `{"key":{},"secret":"lb_sekret_value"}`
	w := adminPost(u, "/app/admin/keys", url.Values{"action": {"create"}, "name": {"ci"}, "expiresDays": {"30"}, "scopes": {"models:read"}})
	out := w.Body.String()
	if got := f.sent[0]; got["name"] != "ci" || got["expiresDays"] != float64(30) || got["scopes"].([]any)[0] != "models:read" {
		t.Errorf("create sent %v", got)
	}
	if !strings.Contains(out, `aria-label="New API key secret" hx-history="false"`) {
		t.Error("the secret fragment can enter htmx history")
	}
	if page := get(u, "/app/admin?section=keys"); page.Header().Get("Cache-Control") != "no-store" {
		t.Error("the Admin page can be cached")
	}
	if !strings.Contains(out, "lb_sekret_value") || !strings.Contains(out, "Copy this secret now") || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("create: %s", out)
	}
	if strings.Contains(get(u, "/app/admin?section=keys").Body.String(), "lb_sekret_value") {
		t.Error("the secret is shown again")
	}
	if out := adminPost(u, "/app/admin/keys", url.Values{"action": {"create"}, "name": {"x"}, "expiresDays": {"1.5"}}).Body.String(); !strings.Contains(out, "whole number of days") {
		t.Error("fractional expiry")
	}
	body := get(u, "/app/admin?section=keys").Body.String()
	if strings.Count(body, ">Rotate</summary>") != 1 || !strings.Contains(body, "The current secret will stop working immediately.") {
		t.Error("only the active key offers a confirmed rotate")
	}
	// Without keys:manage the routes refuse.
	u2, _, _ := adminUI("models:read", "keys:read")
	if w := adminPost(u2, "/app/admin/keys", url.Values{"action": {"revoke"}, "keyId": {"key_1"}}); w.Code != http.StatusForbidden {
		t.Errorf("revoke without keys:manage: %d", w.Code)
	}
}
