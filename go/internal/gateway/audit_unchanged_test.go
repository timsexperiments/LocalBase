package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
)

// A change that changes nothing writes no audit line; a refusal is always audited, and a real
// change still writes its line.
func TestAnUnchangedAccessChangeWritesNoAuditLine(t *testing.T) {
	f := newAuditFixture(t)
	access := func(body string, want int) []map[string]any {
		t.Helper()
		from := len(f.logs.String())
		if status, out := f.send(f.key, "POST", "/_localbase/access-management", "application/json", []byte(body)); status != want {
			t.Fatalf("%s: %d %s, want %d", body, status, out, want)
		}
		return f.auditEvents(from)
	}
	lines := func(events []map[string]any) string {
		var out []string
		for _, ev := range events {
			out = append(out, ev["event"].(string)+" "+ev["action"].(string))
		}
		return strings.Join(out, ", ")
	}

	// Tier 1: turning off what is already off.
	for _, body := range []string{`{"action":"disable"}`, `{"action":"disable-email-delivery"}`} {
		if got := access(body, 200); len(got) != 0 {
			t.Errorf("%s, with nothing to turn off: %s", body, lines(got))
		}
	}
	// A real change still writes its line, and so does turning it off again.
	cloudflare := `{"action":"configure-cloudflare","provider":{"kind":"cloudflare-access","teamDomain":"team.cloudflareaccess.com","audience":"aud"},"origin":"https://lb.example.test","permissions":[]}`
	if got := access(cloudflare, 200); lines(got) != "audit.access-changed configure-cloudflare" {
		t.Errorf("configure: %s", lines(got))
	}
	if got := access(`{"action":"disable"}`, 200); lines(got) != "audit.access-changed disable" {
		t.Errorf("disable: %s", lines(got))
	}
	// A refusal is audited whatever it would have changed.
	if got := access(`{"action":"remove-registration","registrationId":"nobody"}`, 404); lines(got) != "audit.access-refused remove-registration" {
		t.Errorf("a refusal: %s", lines(got))
	}
}

// Tier 2: saving what is already saved writes no line, judged by the cores' changed flags in the
// form the save keeps (permissions and registrations in any order, a policy's roles and bindings in
// any order); a real change writes its line.
func TestSavingWhatIsSavedWritesNoAuditLine(t *testing.T) {
	f := newAuditFixture(t)
	count := func(body string) int {
		t.Helper()
		from := len(f.logs.String())
		status, out := f.send(f.key, "POST", "/_localbase/access-management", "application/json", []byte(body))
		if status != 200 && status != 201 {
			t.Fatalf("%s: %d %s", body, status, out)
		}
		return len(f.auditEvents(from))
	}
	revision := func() string {
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
	oidc := func(name, perms string) string {
		return `{"action":"upsert-oidc","registration":{"kind":"oidc","id":"a","name":"` + name + `","issuer":"https://a.example.test","clientId":"x","clientAuthentication":{"kind":"none"}},"origin":"https://lb.example.test","permissions":[` + perms + `]}`
	}
	email := func(host string) string {
		return `{"action":"configure-email-delivery","config":{"host":"` + host + `","port":587,"security":"starttls","authentication":{"kind":"none"},"from":"sender@example.test"}}`
	}
	roles := gatewaytest.ChatRole + "," + gatewaytest.AdminRole
	owner := `{"kind":"email","role":"admin","email":"owner@example.test"}`
	chat := `{"kind":"email-domain","role":"chat","domain":"example.test"}`

	for _, step := range []struct {
		name  string
		body  func() string
		lines int
	}{
		{"a registration", func() string { return oidc("A", `"inference:chat","models:read"`) }, 1},
		{"the same again", func() string { return oidc("A", `"inference:chat","models:read"`) }, 0},
		{"its permissions in another order", func() string { return oidc("A", `"models:read","inference:chat"`) }, 0},
		{"its name changed", func() string { return oidc("Renamed", `"inference:chat","models:read"`) }, 1},
		{"email delivery", func() string { return email("smtp.example.test") }, 1},
		{"the same again", func() string { return email("smtp.example.test") }, 0},
		{"another host", func() string { return email("mail.example.test") }, 1},
		{"a policy", func() string { return gatewaytest.PolicyRequest(revision(), roles, owner+","+chat, "null") }, 1},
		{"the same again", func() string { return gatewaytest.PolicyRequest(revision(), roles, owner+","+chat, "null") }, 0},
		{"its roles and bindings in another order", func() string {
			return gatewaytest.PolicyRequest(revision(), gatewaytest.AdminRole+","+gatewaytest.ChatRole, chat+","+owner, "null")
		}, 0},
		{"a binding fewer", func() string { return gatewaytest.PolicyRequest(revision(), roles, owner, "null") }, 1},
	} {
		if got := count(step.body()); got != step.lines {
			t.Errorf("%s: %d audit lines, want %d", step.name, got, step.lines)
		}
	}
	cloudflare := func(perms string) string {
		return `{"action":"configure-cloudflare","provider":{"kind":"cloudflare-access","teamDomain":"team.cloudflareaccess.com","audience":"aud"},"origin":"https://lb.example.test","permissions":[` + perms + `]}`
	}
	for _, step := range []struct {
		name  string
		body  string
		lines int
	}{
		{"Cloudflare", cloudflare(`"inference:chat"`), 1},
		{"the same again", cloudflare(`"inference:chat"`), 0},
		{"another permission", cloudflare(`"inference:chat","models:read"`), 1},
	} {
		if got := count(step.body); got != step.lines {
			t.Errorf("%s: %d audit lines, want %d", step.name, got, step.lines)
		}
	}
}

// clear-policy with no policy stored takes a null revision and succeeds, changing nothing (no
// audit line). With a policy, null is a conflict, as is a revision when there is none.
func TestClearingNoPolicySucceeds(t *testing.T) {
	f := newAuditFixture(t)
	send := func(body string, status int, want string) int {
		t.Helper()
		from := len(f.logs.String())
		got, out := f.send(f.key, "POST", "/_localbase/access-management", "application/json", []byte(body))
		if got != status || !strings.Contains(string(out), want) {
			t.Errorf("%s: %d %s, want %d and %s", body, got, out, status, want)
		}
		return len(f.auditEvents(from))
	}
	if n := send(`{"action":"clear-policy","expectedPolicyRevision":null}`, 200, `"cleared":false`); n != 0 {
		t.Errorf("clearing no policy wrote %d audit lines", n)
	}
	send(`{"action":"clear-policy","expectedPolicyRevision":"`+strings.Repeat("0", 64)+`"}`, 409, "policy_revision_conflict")
	send(`{"action":"clear-policy"}`, 400, "validation_failed")

	send(gatewaytest.ConfigureCloudflare, 200, `"restartRequired":true`)
	send(gatewaytest.PolicyRequest("null", gatewaytest.ChatRole+","+gatewaytest.AdminRole,
		`{"kind":"email","role":"admin","email":"owner@example.test"}`, "null"), 200, `"policyRevision"`)
	send(`{"action":"clear-policy","expectedPolicyRevision":null}`, 409, "policy_revision_conflict")
}

// Tier 3: a person's roles set to the same set (in any order), or their status to what it is,
// writes no line; a real change does.
func TestAPersonLeftAsTheyWereWritesNoAuditLine(t *testing.T) {
	f := newAuditFixture(t)
	count := func(body string, want int) int {
		t.Helper()
		from := len(f.logs.String())
		if status, out := f.send(f.key, "POST", "/_localbase/access-management", "application/json", []byte(body)); status != want {
			t.Fatalf("%s: %d %s, want %d", body, status, out, want)
		}
		return len(f.auditEvents(from))
	}
	count(gatewaytest.ConfigureCloudflare, 200)
	count(gatewaytest.PolicyRequest("null", gatewaytest.ChatRole+","+gatewaytest.AdminRole,
		`{"kind":"email","role":"admin","email":"owner@example.test"}`, "null"), 200)
	count(`{"action":"invite-user","email":"friend@example.test","roles":["chat"]}`, 201)
	for _, step := range []struct {
		name, body string
		lines      int
	}{
		{"roles changed", `{"action":"replace-user-roles","email":"friend@example.test","roles":["chat","admin"]}`, 1},
		{"the same roles", `{"action":"replace-user-roles","email":"friend@example.test","roles":["chat","admin"]}`, 0},
		{"the same roles in another order", `{"action":"replace-user-roles","email":"friend@example.test","roles":["admin","chat"]}`, 0},
		{"disabled", `{"action":"disable-user","email":"friend@example.test"}`, 1},
		{"disabled again", `{"action":"disable-user","email":"friend@example.test"}`, 0},
		{"enabled", `{"action":"enable-user","email":"friend@example.test"}`, 1},
		{"enabled again", `{"action":"enable-user","email":"friend@example.test"}`, 0},
		{"removed", `{"action":"remove-user","email":"friend@example.test"}`, 1},
	} {
		if got := count(step.body, 200); got != step.lines {
			t.Errorf("%s: %d audit lines, want %d", step.name, got, step.lines)
		}
	}
}
