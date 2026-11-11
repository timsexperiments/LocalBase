package system

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/test/harness"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// TestCLIAccess runs the same `access` session on twin roots.
// goOwnIssuer is where Go alone records the provider that vouches for an address.
var goOwnIssuer = regexp.MustCompile(`^\.data\.(user|users\[\d+\]|policy\.bindings\[[01]\])\.issuer$`)

func TestCLIAccess(t *testing.T) {
	harness.RequireTS(t)
	ts, gr := twinRoots(t)
	roots := [2]string{ts, gr}
	t.Setenv("CONTRACT_OIDC_SECRET", "shh")
	t.Setenv("CONTRACT_SMTP_PASSWORD", "pw")
	dir := fsguard.ScratchDir(t)
	policy := filepath.Join(dir, "policy.json")
	policyText := `{"roles":[{"name":"chat","description":"Chat","permissions":["inference:chat"]},` +
		`{"name":"admin","description":"Admin","permissions":["access:manage","access:read"]}],` +
		`"bindings":[{"kind":"email","role":"admin","email":"owner@example.com"},{"kind":"email-domain","role":"chat","domain":"example.com"}],"defaultRole":null}`
	fsguard.WriteFile(policy, []byte(policyText), 0o600)
	// With several providers Go wants to know which one vouches for an address, and TS has no
	// such field (DESIGN §13.6, item 5b).
	const vouching = "https://accounts.google.com"
	goPolicy := filepath.Join(dir, "go-policy.json")
	fsguard.WriteFile(goPolicy, []byte(strings.NewReplacer(`"kind":"email",`, `"kind":"email","issuer":"`+vouching+`",`,
		`"kind":"email-domain",`, `"kind":"email-domain","issuer":"`+vouching+`",`).Replace(policyText)), 0o600)
	badPolicy := filepath.Join(dir, "bad.json")
	fsguard.WriteFile(badPolicy, []byte(`{"roles":[],"bindings":[],"defaultRole":null,"extra":1}`), 0o600)
	notJSON := filepath.Join(dir, "not.json")
	fsguard.WriteFile(notJSON, []byte(`{`), 0o600)
	msg := func(p string) bool { return p == ".error.message" }
	origin := []string{"--origin", "https://lb.example.com"}
	with := func(args ...string) []string { return append(append([]string{"--json", "access"}, args...), origin...) }
	for _, c := range []cliCase{
		{args: []string{"--json", "init"}},
		{args: []string{"--json", "access", "show"}},
		{args: []string{"--json", "access", "email", "show"}},
		{args: []string{"--json", "access", "policy", "show"}},
		{args: []string{"--json", "access", "users", "list"}},
		{args: []string{"--json", "access", "oidc", "list"}},
		{args: []string{"--json", "access", "policy", "test", "--issuer", "https://id.example.com", "--subject", "s"}},
		{args: []string{"--json", "access", "policy", "clear"}},
		{args: []string{"--json", "access", "users", "invite", "--email", "a@example.com", "--roles", "chat"}},
		{args: []string{"--json", "access", "policy", "apply", "--file", policy}, goArgs: []string{"--json", "access", "policy", "apply", "--file", goPolicy}},
		{args: []string{"--json", "access", "magic-link", "add", "--id", "email", "--name", "Email", "--origin", "https://lb.example.com"}},
		{args: []string{"--json", "access", "cloudflare", "--team-domain", "team.cloudflareaccess.com", "--audience", "aud"}, skip: msg},
		{args: with("cloudflare", "--team-domain", "Bad", "--audience", "aud"), skip: msg},
		{args: []string{"--json", "access", "cloudflare", "--team-domain", "team.cloudflareaccess.com", "--audience", "aud", "--origin", "http://x"}, skip: msg},
		{args: with("cloudflare", "--team-domain", "team.cloudflareaccess.com", "--audience", "aud", "--permissions", "bogus"), skip: msg},
		// With no --permissions a new provider grants nothing on Go and TS's default on TS
		// (DESIGN §13.6); given the same list, everything else must match.
		{args: with("cloudflare", "--team-domain", "team.cloudflareaccess.com", "--audience", "aud"),
			goArgs: with("cloudflare", "--team-domain", "team.cloudflareaccess.com", "--audience", "aud", "--permissions", "inference:chat,models:read")},
		{args: []string{"--json", "access", "show"}},
		{args: with("oidc", "add", "--id", "google", "--name", "Google", "--issuer", "https://accounts.google.com", "--client-id", "cid")},
		{args: with("oidc", "add", "--id", "google", "--name", "Google", "--issuer", "https://accounts.google.com", "--client-id", "cid", "--client-secret-env", "MISSING_SECRET_VAR")},
		{args: with("oidc", "add", "--id", "google", "--name", "Google", "--issuer", "https://accounts.google.com", "--client-id", "cid", "--client-secret-env", "CONTRACT_OIDC_SECRET", "--permissions", "models:read,inference:chat")},
		{args: with("oidc", "add", "--id", "pub", "--name", "Public", "--issuer", "https://id.example.com", "--client-id", "pc", "--public-client")},
		{args: []string{"--json", "access", "oidc", "list"}},
		{args: []string{"--json", "access", "show"}},
		{args: []string{"--json", "access", "email", "configure", "--host", "127.0.0.1", "--port", "1", "--security", "starttls", "--from", "lb@example.com", "--username", "u"}},
		{args: []string{"--json", "access", "email", "configure", "--host", "127.0.0.1", "--port", "1", "--security", "ssl", "--from", "lb@example.com"}, skip: msg},
		{args: []string{"--json", "access", "email", "configure", "--host", "127.0.0.1", "--port", "1", "--security", "starttls", "--from", "LB@example.com", "--username", "u", "--password-env", "CONTRACT_SMTP_PASSWORD"}},
		{args: []string{"--json", "access", "email", "show"}},
		{args: []string{"--json", "access", "magic-link", "add", "--id", "email", "--name", "Email", "--origin", "https://lb.example.com"}},
		{args: []string{"--json", "access", "magic-link", "add", "--id", "email2", "--name", "Email", "--origin", "https://lb.example.com"}},
		{args: []string{"--json", "access", "magic-link", "list"}},
		{args: []string{"--json", "access", "email", "disable"}},
		{args: []string{"--json", "access", "policy", "apply", "--file", notJSON}},
		{args: []string{"--json", "access", "policy", "apply", "--file", filepath.Join(dir, "missing.json")}, skip: msg},
		{args: []string{"--json", "access", "policy", "apply", "--file", badPolicy}, skip: msg},
		{args: []string{"--json", "access", "policy", "apply", "--file", policy}, goArgs: []string{"--json", "access", "policy", "apply", "--file", goPolicy}},
		{args: []string{"--json", "access", "policy", "show"}},
		{args: []string{"--json", "access", "policy", "test", "--issuer", "https://accounts.google.com", "--subject", "s", "--email", "Owner@Example.com"}},
		{args: []string{"--json", "access", "policy", "test", "--issuer", "https://accounts.google.com", "--subject", "s"}},
		{args: []string{"--json", "access", "users", "invite", "--email", "Friend@Example.com", "--roles", "chat"}, goArgs: []string{"--json", "access", "users", "invite", "--email", "Friend@Example.com", "--roles", "chat", "--issuer", vouching}},
		{args: []string{"--json", "access", "users", "invite", "--email", "friend@example.com", "--roles", "chat"}, goArgs: []string{"--json", "access", "users", "invite", "--email", "friend@example.com", "--roles", "chat", "--issuer", vouching}},
		{args: []string{"--json", "access", "users", "invite", "--email", "x@example.com", "--roles", ""}, skip: msg},
		{args: []string{"--json", "access", "users", "invite", "--email", "x@example.com", "--roles", "missing"}, goArgs: []string{"--json", "access", "users", "invite", "--email", "x@example.com", "--roles", "missing", "--issuer", vouching}},
		{args: []string{"--json", "access", "users", "invite", "--email", "nope", "--roles", "chat"}, skip: msg},
		{args: []string{"--json", "access", "users", "roles", "--email", "friend@example.com", "--roles", "admin,chat"}},
		{args: []string{"--json", "access", "users", "roles", "--email", "friend@example.com", "--roles", ""}},
		{args: []string{"--json", "access", "users", "disable", "--email", "friend@example.com"}},
		{args: []string{"--json", "access", "users", "enable", "--email", "friend@example.com"}},
		{args: []string{"--json", "access", "users", "enable", "--email", "ghost@example.com"}},
		{args: []string{"--json", "access", "users", "list"}},
		{args: []string{"--json", "access", "users", "remove", "--email", "friend@example.com"}},
		{args: []string{"--json", "access", "policy", "clear"}},
		{args: []string{"--json", "access", "policy", "clear"}},
		{args: []string{"--json", "access", "oidc", "remove", "email"}},
		{args: []string{"--json", "access", "magic-link", "remove", "email"}},
		{args: []string{"--json", "access", "email", "disable"}},
		{args: []string{"--json", "access", "email", "disable"}},
		{args: []string{"--json", "access", "github", "list"}},
		{args: []string{"--json", "access", "oidc", "remove", "google"}},
		{args: []string{"--json", "access", "oidc", "remove", "pub"}},
		{args: []string{"--json", "access", "show"}},
		{args: []string{"--json", "access", "disable"}},
		{args: []string{"--json", "access", "oidc", "remove"}, skip: msg},
	} {
		own := c.skip
		c.skip = func(p string) bool { return goOwnIssuer.MatchString(p) || (own != nil && own(p)) }
		compareCLI(t, roots, c)
	}
}
