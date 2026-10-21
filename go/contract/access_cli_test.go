package contract

import (
	"path/filepath"
	"testing"
)

// TestCLIAccess runs the same `access` session on twin roots.
func TestCLIAccess(t *testing.T) {
	requireTS(t)
	ts, gr := twinRoots(t)
	roots := [2]string{ts, gr}
	t.Setenv("CONTRACT_OIDC_SECRET", "shh")
	t.Setenv("CONTRACT_SMTP_PASSWORD", "pw")
	dir := scratchDir(t)
	policy := filepath.Join(dir, "policy.json")
	mustWriteFile(policy, []byte(`{"roles":[{"name":"chat","description":"Chat","permissions":["inference:chat"]},`+
		`{"name":"admin","description":"Admin","permissions":["access:manage","access:read"]}],`+
		`"bindings":[{"kind":"email","role":"admin","email":"owner@example.com"},{"kind":"email-domain","role":"chat","domain":"example.com"}],"defaultRole":null}`), 0o600)
	badPolicy := filepath.Join(dir, "bad.json")
	mustWriteFile(badPolicy, []byte(`{"roles":[],"bindings":[],"defaultRole":null,"extra":1}`), 0o600)
	notJSON := filepath.Join(dir, "not.json")
	mustWriteFile(notJSON, []byte(`{`), 0o600)
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
		{args: []string{"--json", "access", "policy", "apply", "--file", policy}},
		{args: []string{"--json", "access", "magic-link", "add", "--id", "email", "--name", "Email", "--origin", "https://lb.example.com"}},
		{args: []string{"--json", "access", "cloudflare", "--team-domain", "team.cloudflareaccess.com", "--audience", "aud"}, skip: msg},
		{args: with("cloudflare", "--team-domain", "Bad", "--audience", "aud"), skip: msg},
		{args: []string{"--json", "access", "cloudflare", "--team-domain", "team.cloudflareaccess.com", "--audience", "aud", "--origin", "http://x"}, skip: msg},
		{args: with("cloudflare", "--team-domain", "team.cloudflareaccess.com", "--audience", "aud", "--permissions", "bogus"), skip: msg},
		{args: with("cloudflare", "--team-domain", "team.cloudflareaccess.com", "--audience", "aud")},
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
		{args: []string{"--json", "access", "policy", "apply", "--file", policy}},
		{args: []string{"--json", "access", "policy", "show"}},
		{args: []string{"--json", "access", "policy", "test", "--issuer", "https://accounts.google.com", "--subject", "s", "--email", "Owner@Example.com"}},
		{args: []string{"--json", "access", "policy", "test", "--issuer", "https://accounts.google.com", "--subject", "s"}},
		{args: []string{"--json", "access", "users", "invite", "--email", "Friend@Example.com", "--roles", "chat"}},
		{args: []string{"--json", "access", "users", "invite", "--email", "friend@example.com", "--roles", "chat"}},
		{args: []string{"--json", "access", "users", "invite", "--email", "x@example.com", "--roles", ""}, skip: msg},
		{args: []string{"--json", "access", "users", "invite", "--email", "x@example.com", "--roles", "missing"}},
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
		compareCLI(t, roots, c)
	}
}
