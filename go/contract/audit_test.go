package contract

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Admin changes through a real Go gateway leave audit events with actor and
// target ids, and the planted secrets of those changes (a client secret, an
// SMTP password, invited emails, issued and rotated key secrets) appear in
// no event: the planted-secret scan, extended to audit events.
func TestAdminAuditRedaction(t *testing.T) {
	if *implFlag != "go" && *implFlag != "both" {
		t.Skip("runs the Go gateway")
	}
	tmp := scratchDir(t)
	e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp, noAuth: true}
	if r := e.cli(nil, "--json", "init"); r.code != 0 {
		t.Fatalf("init: %s %s", r.stdout, r.stderr)
	}
	e.serve("--no-auth")
	defer e.stop()
	secrets := []string{"plantedClientSecretZ93", "plantedSmtpPasswordQ71", "invitee.planted@example.org", "owner.planted@example.org"}
	post := func(path, body string, status int) string {
		t.Helper()
		r := e.do("POST", path, body)
		if r.Status != status {
			t.Fatalf("%s: %d %s", body, r.Status, r.Body)
		}
		return string(r.Body)
	}
	const access = "/_localbase/access-management"
	post(access, `{"action":"upsert-oidc","registration":{"kind":"oidc","id":"google","name":"Google","issuer":"https://accounts.google.com","clientId":"cid","clientAuthentication":{"kind":"client-secret-basic","clientSecret":"plantedClientSecretZ93"}},"origin":"https://lb.example.com","permissions":["inference:chat"]}`, 200)
	post(access, `{"action":"configure-email-delivery","config":{"host":"127.0.0.1","port":2525,"security":"starttls","authentication":{"kind":"password","username":"u","password":"plantedSmtpPasswordQ71"},"from":"lb@example.org"}}`, 200)
	post(access, `{"action":"apply-policy","policy":{"roles":[{"name":"admin","description":"","permissions":["access:manage","access:read"]},{"name":"chat","description":"","permissions":["inference:chat"]}],"bindings":[{"kind":"email","role":"admin","email":"owner.planted@example.org"}],"defaultRole":null},"expectedPolicyRevision":null}`, 200)
	post(access, `{"action":"invite-user","email":"invitee.planted@example.org","roles":["chat"],"sendEmail":false}`, 201)
	post(access, `{"action":"replace-user-roles","email":"invitee.planted@example.org","roles":["chat","admin"]}`, 200)
	const keys = "/_localbase/api-keys"
	created := post(keys, `{"action":"create","name":"audit","scopes":["models:read"]}`, 201)
	id := regexp.MustCompile(`"id":"(key_[0-9a-f-]+)"`).FindStringSubmatch(created)[1]
	rotated := post(keys, `{"action":"rotate","keyId":"`+id+`"}`, 200)
	post(keys, `{"action":"revoke","keyId":"`+id+`"}`, 200)
	for _, body := range []string{created, rotated} {
		secrets = append(secrets, regexp.MustCompile(`"secret":"([^"]+)"`).FindStringSubmatch(body)[1])
	}
	e.stop()

	b, err := os.ReadFile(filepath.Join(e.root, "logs", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(b)
	for _, s := range secrets {
		if strings.Contains(log, s) {
			t.Errorf("the event log contains a planted value (%d chars)", len(s))
		}
	}
	for _, want := range []string{`"action":"upsert-oidc"`, `"target_kind":"registration"`, `"target_id":"google"`, `"target_kind":"email-delivery"`,
		`"target_kind":"policy"`, `"action":"invite-user"`, `"target_kind":"user"`, `"action":"replace-user-roles"`,
		`"eventName":"audit.api-key-changed"`, `"action":"rotate"`, `"target_id":"` + id + `"`, `"actor_kind":"environment"`} {
		if !strings.Contains(log, want) {
			t.Errorf("audit events: missing %s", want)
		}
	}
	if n := strings.Count(log, `"eventName":"audit.`); n != 8 {
		t.Errorf("%d audit events, want 8", n)
	}
}
