package gateway

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// accessOp is one step of an access golden: a request to the access route.
type accessOp struct{ name, body string }

// runAccessGolden sends each step through the gateway and records, for each, what a person sees
// and what is left behind: the status, the answer, the saved ui-access.json and
// email-delivery.json, and whether an audit line was written. It compares that with
// testdata/<file>, or rewrites it under LOCALBASE_UPDATE_ACCESS_GOLDEN=1. Each state function adds a
// line of its own after each step (the stored policy, the people). A switch to the auth
// cores must leave every line of it as it was.
func runAccessGolden(t *testing.T, file string, steps []accessOp, state ...func(*auditFixture) string) {
	t.Helper()
	f := newAuditFixture(t)
	f.srv.changeAudit.sampler.Burst = 1000
	saved := func(name string) string {
		b, err := os.ReadFile(filepath.Join(f.srv.cfg.Root.Dir, name))
		if os.IsNotExist(err) {
			return "(none)"
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	var out bytes.Buffer
	for _, s := range steps {
		request := s.body
		if strings.Contains(request, "{{rev}}") { // the stored policy's revision, as JSON
			request = strings.ReplaceAll(request, "{{rev}}", policyRevision(t, f))
		}
		from := len(f.logs.String())
		status, body := f.send(f.key, "POST", "/_localbase/access-management", "application/json", []byte(request))
		var answer bytes.Buffer
		if json.Indent(&answer, bytes.TrimSpace(body), "  ", "  ") != nil {
			answer.Reset()
			answer.Write(body)
		}
		audit := []string{}
		for _, ev := range f.auditEvents(from) {
			audit = append(audit, ev["event"].(string)+" "+ev["action"].(string))
		}
		out.WriteString("== " + s.name + "\n")
		out.WriteString("request: " + s.body + "\n")
		out.WriteString("status: " + itoa(status) + "\n")
		out.WriteString("answer: " + answer.String() + "\n")
		out.WriteString("audit: " + strings.Join(audit, ", ") + "\n")
		out.WriteString("ui-access.json: " + saved("ui-access.json") + "\n")
		out.WriteString("email-delivery.json: " + saved("email-delivery.json") + "\n")
		for _, s := range state { // what lives in the database, for the policy and users goldens
			out.WriteString(s(f) + "\n")
		}
		out.WriteString("\n")
	}
	path := filepath.Join("testdata", file)
	if os.Getenv("LOCALBASE_UPDATE_ACCESS_GOLDEN") == "1" {
		if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (LOCALBASE_UPDATE_ACCESS_GOLDEN=1 writes it)", err)
	}
	if got := out.String(); got != string(want) {
		wl, gl := strings.Split(string(want), "\n"), strings.Split(got, "\n")
		for i := 0; i < len(wl) || i < len(gl); i++ {
			var w, g string
			if i < len(wl) {
				w = wl[i]
			}
			if i < len(gl) {
				g = gl[i]
			}
			if w != g {
				t.Fatalf("%s differs at line %d:\nwant %s\ngot  %s", path, i+1, w, g)
			}
		}
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// Every provider action, as the gateway answers it and what it leaves on disk.
func TestProviderActionsGolden(t *testing.T) {
	const origin = `"origin":"https://lb.example.test"`
	oidc := func(id, name, issuer, perms string) string {
		return `{"action":"upsert-oidc","registration":{"kind":"oidc","id":"` + id + `","name":"` + name + `","issuer":"` + issuer +
			`","clientId":"client","clientAuthentication":{"kind":"client-secret-basic","clientSecret":"PLANTED-oidc-secret"}},` + origin + `,"permissions":[` + perms + `]}`
	}
	magic := func(id string) string {
		return `{"action":"upsert-magic-link","registration":{"kind":"magic-link","id":"` + id + `","name":"Email"},` + origin + `,"permissions":["inference:chat","access:manage","access:read"]}`
	}
	cloudflare := func(perms string) string {
		return `{"action":"configure-cloudflare","provider":{"kind":"cloudflare-access","teamDomain":"team.cloudflareaccess.com","audience":"aud"},` + origin + `,"permissions":[` + perms + `]}`
	}
	runAccessGolden(t, "access_providers.golden", []accessOp{
		{"disable with nothing configured", `{"action":"disable"}`},
		{"remove from nothing", `{"action":"remove-registration","registrationId":"a"}`},
		{"Cloudflare", cloudflare(`"inference:chat","access:manage","access:read"`)},
		{"Cloudflare again, permissions reordered", cloudflare(`"access:read","inference:chat","access:manage"`)},
		{"remove from Cloudflare", `{"action":"remove-registration","registrationId":"a"}`},
		{"OpenID Connect replaces Cloudflare", oidc("b", "B", "https://b.example.test", `"inference:chat","access:manage","access:read"`)},
		{"a second OpenID Connect", oidc("a", "A", "https://a.example.test", `"models:read","inference:chat","access:manage","access:read"`)},
		{"the same again, reordered", oidc("a", "A", "https://a.example.test", `"access:read","access:manage","inference:chat","models:read"`)},
		{"renamed", oidc("a", "Renamed", "https://a.example.test", `"models:read","inference:chat","access:manage","access:read"`)},
		{"GitHub", `{"action":"upsert-github","registration":{"kind":"github-oauth","id":"gh","name":"GitHub","clientId":"c","clientSecret":"PLANTED-github-secret"},` + origin + `,"permissions":["inference:chat","access:manage","access:read"]}`},
		{"magic link without email delivery", magic("email")},
		{"email delivery", `{"action":"configure-email-delivery","config":{"host":"smtp.example.test","port":587,"security":"starttls","authentication":{"kind":"none"},"from":"sender@example.test"}}`},
		{"magic link", magic("email")},
		{"the same magic link again", magic("email")},
		{"a second magic link", magic("email2")},
		{"remove an unknown one", `{"action":"remove-registration","registrationId":"nobody"}`},
		{"remove GitHub", `{"action":"remove-registration","registrationId":"gh"}`},
		{"remove a", `{"action":"remove-registration","registrationId":"a"}`},
		{"remove b", `{"action":"remove-registration","registrationId":"b"}`},
		{"remove the last, the magic link", `{"action":"remove-registration","registrationId":"email"}`},
		{"disable when already off", `{"action":"disable"}`},
		{"Cloudflare once more", cloudflare(`"inference:chat","access:manage","access:read"`)},
		{"disable", `{"action":"disable"}`},
	})
}

// Every email delivery action, as the gateway answers it and what it leaves on disk. (Sending a
// test email to a real server isn't here: the golden records only what needs no network.)
func TestEmailActionsGolden(t *testing.T) {
	email := func(host string) string {
		return `{"action":"configure-email-delivery","config":{"host":"` + host + `","port":587,"security":"starttls","authentication":{"kind":"password","username":"user","password":"PLANTED-smtp-password"},"from":"sender@example.test"}}`
	}
	runAccessGolden(t, "access_email.golden", []accessOp{
		{"a test email with none configured", `{"action":"test-email-delivery","to":"friend@example.test"}`},
		{"disable with none configured", `{"action":"disable-email-delivery"}`},
		{"configure", email("smtp.example.test")},
		{"the same again", email("smtp.example.test")},
		{"another host", email("mail.example.test")},
		{"a magic link that needs it", `{"action":"upsert-magic-link","registration":{"kind":"magic-link","id":"email","name":"Email"},"origin":"https://lb.example.test","permissions":["inference:chat","access:manage","access:read"]}`},
		{"disable while the magic link needs it", `{"action":"disable-email-delivery"}`},
		{"remove the magic link", `{"action":"remove-registration","registrationId":"email"}`},
		{"disable", `{"action":"disable-email-delivery"}`},
		{"disable again", `{"action":"disable-email-delivery"}`},
	})
}

// policyRevision is the stored policy's revision as JSON ("null" for none), from the access GET.
func policyRevision(t *testing.T, f *auditFixture) string {
	t.Helper()
	_, out := f.send(f.key, "GET", "/_localbase/access-management", "", nil)
	var v struct {
		Revision *string `json:"policyRevision"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatal(err)
	}
	if v.Revision == nil {
		return "null"
	}
	return `"` + *v.Revision + `"`
}

// storedPolicy is the stored policy and its revision, from the access GET, for a golden.
func storedPolicy(f *auditFixture) string {
	_, out := f.send(f.key, "GET", "/_localbase/access-management", "", nil)
	var v struct {
		Policy   json.RawMessage `json:"policy"`
		Revision json.RawMessage `json:"policyRevision"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return "policy: (unreadable answer) " + string(out)
	}
	return "policy: " + string(v.Policy) + "\npolicyRevision: " + string(v.Revision)
}

// Every policy action, as the gateway answers it and what it leaves stored.
func TestPolicyActionsGolden(t *testing.T) {
	const roles = `{"name":"chat","description":"","permissions":["inference:chat"]},{"name":"admin","description":"","permissions":["access:read","access:manage"]}`
	const rolesReordered = `{"name":"admin","description":"","permissions":["access:manage","access:read"]},{"name":"chat","description":"","permissions":["inference:chat"]}`
	const owner = `{"kind":"email","role":"admin","email":"Owner@Example.test"}`
	const domain = `{"kind":"email-domain","role":"chat","domain":"example.test"}`
	// With two providers, a binding must name the one that vouches for it.
	const ownerOnA = `{"kind":"email","role":"admin","email":"owner@example.test","issuer":"https://a.example.test"}`
	const domainOnA = `{"kind":"email-domain","role":"chat","domain":"example.test","issuer":"https://a.example.test"}`
	apply := func(rev, roles, bindings string) string {
		return `{"action":"apply-policy","expectedPolicyRevision":` + rev + `,"policy":{"roles":[` + roles + `],"bindings":[` + bindings + `],"defaultRole":null}}`
	}
	test := func(subject, email string) string {
		return `{"action":"test-policy","identity":{"issuer":"https://a.example.test","subject":"` + subject + `","verifiedEmail":"` + email + `"}}`
	}
	oidc := func(id, issuer string) string {
		return `{"action":"upsert-oidc","registration":{"kind":"oidc","id":"` + id + `","name":"P","issuer":"` + issuer + `","clientId":"x","clientAuthentication":{"kind":"none"}},"origin":"https://lb.example.test","permissions":["inference:chat","access:manage","access:read"]}`
	}
	runAccessGolden(t, "access_policy.golden", []accessOp{
		{"test with no provider", test("s", "owner@example.test")},
		{"apply with no provider", apply("null", roles, owner)},
		{"clear with no policy", `{"action":"clear-policy","expectedPolicyRevision":null}`},
		{"a provider", oidc("a", "https://a.example.test")},
		{"test with no policy", test("s", "owner@example.test")},
		{"apply, the binding naming no provider", apply("{{rev}}", roles, owner+","+domain)},
		{"the same again", apply("{{rev}}", roles, owner+","+domain)},
		{"reordered", apply("{{rev}}", rolesReordered, domain+","+owner)},
		{"test the owner", test("s", "owner@example.test")},
		{"test someone in the domain", test("t", "friend@example.test")},
		{"test a stranger", test("u", "stranger@elsewhere.test")},
		{"a new binding on an unknown provider", apply("{{rev}}", roles, owner+","+domain+`,{"kind":"email","role":"chat","email":"new@example.test","issuer":"https://c.example.test"}`)},
		{"a second provider", oidc("b", "https://b.example.test")},
		{"a new binding naming none, with two", apply("{{rev}}", roles, owner+","+domain+`,{"kind":"email","role":"chat","email":"new@example.test"}`)},
		{"a stale revision", apply(`"`+strings.Repeat("0", 64)+`"`, roles, ownerOnA+","+domainOnA)},
		{"the same, named, with two", apply("{{rev}}", roles, ownerOnA+","+domainOnA)},
		{"a binding fewer", apply("{{rev}}", roles, ownerOnA)},
		{"clear", `{"action":"clear-policy","expectedPolicyRevision":{{rev}}}`},
		{"clear again", `{"action":"clear-policy","expectedPolicyRevision":null}`},
	}, storedPolicy)
}
