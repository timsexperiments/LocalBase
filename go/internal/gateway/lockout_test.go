package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Access management refuses changes that lock the caller, or everyone, out
// of access:manage (a Go addition the operator chose over TS's UI-only
// checks), and audits each change without emails or secrets.
func TestAccessLockout(t *testing.T) {
	r, err := root.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var logs bytes.Buffer
	s := New(&Server{Store: st, Root: r, Auth: &auth.Resolver{Store: st, Mode: "either"}, Log: slog.New(slog.NewJSONHandler(&logs, nil))})

	host := &auth.Principal{Kind: "environment", Permissions: auth.Permissions}
	browser := func(email string) *auth.Principal {
		return &auth.Principal{Kind: "browser-session", ID: "browser:" + strings.Split(email, "@")[0] + "-hash", Name: email, Permissions: auth.Permissions,
			Identity: &auth.Identity{Issuer: "https://lb.example.com/magic-link", Subject: email, VerifiedEmail: email}}
	}
	owner, second := browser("owner@example.com"), browser("second@example.com")
	call := func(p *auth.Principal, body string) (int, string) {
		t.Helper()
		res := s.Call(context.Background(), p, "POST", "/_localbase/access-management", "application/json", []byte(body))
		return res.Status, string(res.Body)
	}
	expect := func(p *auth.Principal, body string, status int, code string) {
		t.Helper()
		got, out := call(p, body)
		if got != status || (code != "" && !strings.Contains(out, `"code":"`+code+`"`)) {
			t.Fatalf("%s: %d %s, want %d %s", body, got, out, status, code)
		}
	}
	revision := func() string {
		t.Helper()
		res := s.Call(context.Background(), host, "GET", "/_localbase/access-management", "", nil)
		var v struct{ PolicyRevision *string }
		json.Unmarshal(res.Body, &v)
		if v.PolicyRevision == nil {
			return "null"
		}
		return `"` + *v.PolicyRevision + `"`
	}
	policy := func(admin string) string {
		return `{"roles":[{"name":"admin","description":"","permissions":["access:read","access:manage"]},{"name":"chat","description":"","permissions":["inference:chat"]}],` +
			`"bindings":[{"kind":"email","role":"admin","email":"` + admin + `"}],"defaultRole":null}`
	}

	expect(host, `{"action":"configure-cloudflare","provider":{"kind":"cloudflare-access","teamDomain":"team.cloudflareaccess.com","audience":"aud"},"origin":"https://lb.example.com","permissions":["inference:chat"]}`, 200, "")
	expect(host, `{"action":"apply-policy","policy":`+policy("owner@example.com")+`,"expectedPolicyRevision":null}`, 200, "")
	expect(host, `{"action":"invite-user","email":"owner@example.com","roles":["admin"]}`, 201, "")
	expect(host, `{"action":"invite-user","email":"second@example.com","roles":["chat"]}`, 201, "")

	// Your own record: never disabled or forgotten by you, whatever else holds.
	expect(owner, `{"action":"disable-user","email":"Owner@Example.com"}`, 409, "would_lock_out_admins")
	expect(owner, `{"action":"remove-user","email":"owner@example.com"}`, 409, "would_lock_out_admins")
	// Moving the binding to second keeps the owner an admin through their role.
	expect(owner, `{"action":"apply-policy","policy":`+policy("second@example.com")+`,"expectedPolicyRevision":`+revision()+`}`, 200, "")
	// Dropping that role would leave the owner without access:manage.
	expect(owner, `{"action":"replace-user-roles","email":"owner@example.com","roles":["chat"]}`, 409, "would_lock_out_admins")
	// Others may change the owner, and the owner others, while a manager remains.
	expect(second, `{"action":"replace-user-roles","email":"owner@example.com","roles":["chat"]}`, 200, "")
	expect(owner, `{"action":"enable-user","email":"owner@example.com"}`, 200, "")
	// Disabling the last manager is refused for anyone, keys and the host too.
	expect(host, `{"action":"disable-user","email":"second@example.com"}`, 409, "would_lock_out_admins")
	expect(host, `{"action":"remove-user","email":"second@example.com"}`, 200, "") // the binding still reaches second
	// A policy whose only manager role is unreachable is TS's own error first.
	expect(host, `{"action":"apply-policy","policy":{"roles":[{"name":"chat","description":"","permissions":["inference:chat"]}],"bindings":[],"defaultRole":"chat"},"expectedPolicyRevision":`+revision()+`}`, 400, "")
	// TS's refusal to clear a policy with managed users is unchanged.
	expect(host, `{"action":"clear-policy","expectedPolicyRevision":`+revision()+`}`, 409, "policy_conflict")
	expect(host, `{"action":"remove-user","email":"owner@example.com"}`, 200, "")
	// With no managed users, clearing would hand everyone provider-wide
	// permissions without access:manage: refused.
	expect(host, `{"action":"clear-policy","expectedPolicyRevision":`+revision()+`}`, 409, "would_lock_out_admins")

	out := logs.String()
	if strings.Count(out, `"audit.access-changed"`) < 8 || !strings.Contains(out, `"action":"apply-policy"`) || !strings.Contains(out, `"actor_kind":"browser-session"`) {
		t.Errorf("audit events:\n%s", out)
	}
	if !strings.Contains(out, `"target_kind":"user","target_id":"`) || !strings.Contains(out, `"target_kind":"policy","target_id":"`) || !strings.Contains(out, `"target_kind":"provider","target_id":"browser-access"`) {
		t.Errorf("audit targets:\n%s", out)
	}
	if strings.Contains(out, "@example.com") || strings.Contains(out, `"action":"disable-user"`) {
		t.Errorf("the audit log has an email, or a refused change:\n%s", out)
	}
}

// API keys with access:manage are managing principals too: a key can't
// revoke itself or drop its own access:manage, and nobody may revoke or
// rescope the last one when no person can manage access.
func TestKeyLockout(t *testing.T) {
	r, err := root.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := New(&Server{Store: st, Root: r, Auth: &auth.Resolver{Store: st, Mode: "either"}, Log: slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))})
	k1, _, _ := st.CreateAPIKey(context.Background(), "one", []string{"keys:manage", "access:manage"})
	k2, _, _ := st.CreateAPIKey(context.Background(), "two", []string{"keys:manage", "access:manage"})
	plain, _, _ := st.CreateAPIKey(context.Background(), "plain", []string{"inference:chat"})
	host := &auth.Principal{Kind: "environment", Permissions: auth.Permissions}
	asKey := func(id string) *auth.Principal {
		return &auth.Principal{Kind: "api-key", ID: id, Permissions: auth.Permissions}
	}
	expect := func(p *auth.Principal, body string, status int) {
		t.Helper()
		res := s.Call(context.Background(), p, "POST", "/_localbase/api-keys", "application/json", []byte(body))
		if res.Status != status || (status == 409 && !strings.Contains(string(res.Body), `"code":"would_lock_out_admins"`)) {
			t.Fatalf("%s: %d %s, want %d", body, res.Status, res.Body, status)
		}
	}
	expect(asKey(k1.ID), `{"action":"revoke","keyId":"`+k1.ID+`"}`, 409)
	expect(asKey(k1.ID), `{"action":"set-scopes","keyId":"`+k1.ID+`","scopes":["keys:manage"]}`, 409)
	expect(asKey(k1.ID), `{"action":"rotate","keyId":"`+k1.ID+`"}`, 200) // rotating your own key is fine
	expect(asKey(k1.ID), `{"action":"revoke","keyId":"`+plain.ID+`"}`, 200)
	expect(host, `{"action":"revoke","keyId":"`+k2.ID+`"}`, 200) // k1 still manages
	expect(host, `{"action":"set-scopes","keyId":"`+k1.ID+`","scopes":["keys:manage"]}`, 409)
	expect(host, `{"action":"revoke","keyId":"`+k1.ID+`"}`, 409)
	expect(host, `{"action":"set-scopes","keyId":"`+k1.ID+`","scopes":["keys:manage","access:manage","models:read"]}`, 200)
}
