package system

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// TestCLIAccess pins an `access` session.
func TestCLIAccess(t *testing.T) {
	pinned(t)
	gr := cliRoot(t)
	t.Setenv("CONTRACT_OIDC_SECRET", "shh")
	t.Setenv("CONTRACT_SMTP_PASSWORD", "pw")
	dir := fsguard.ScratchDir(t)
	// With several providers, each binding names the one that vouches for its address (DESIGN
	// §13.6, item 5b). The file keeps the name it had beside TS's policy, which had no such field.
	const vouching = "https://accounts.google.com"
	goPolicy := filepath.Join(dir, "go-policy.json")
	fsguard.WriteFile(goPolicy, []byte(`{"roles":[{"name":"chat","description":"Chat","permissions":["inference:chat"]},`+
		`{"name":"admin","description":"Admin","permissions":["access:manage","access:read"]}],`+
		`"bindings":[{"kind":"email","issuer":"`+vouching+`","role":"admin","email":"owner@example.com"},`+
		`{"kind":"email-domain","issuer":"`+vouching+`","role":"chat","domain":"example.com"}],"defaultRole":null}`), 0o600)
	badPolicy := filepath.Join(dir, "bad.json")
	fsguard.WriteFile(badPolicy, []byte(`{"roles":[],"bindings":[],"defaultRole":null,"extra":1}`), 0o600)
	notJSON := filepath.Join(dir, "not.json")
	fsguard.WriteFile(notJSON, []byte(`{`), 0o600)
	origin := []string{"--origin", "https://lb.example.com"}
	with := func(args ...string) []string { return append(append([]string{"--json", "access"}, args...), origin...) }
	for _, args := range [][]string{
		{"--json", "init"},
		{"--json", "access", "show"},
		{"--json", "access", "email", "show"},
		{"--json", "access", "policy", "show"},
		{"--json", "access", "users", "list"},
		{"--json", "access", "oidc", "list"},
		{"--json", "access", "policy", "test", "--issuer", "https://id.example.com", "--subject", "s"},
		{"--json", "access", "users", "invite", "--email", "a@example.com", "--roles", "chat"},
		{"--json", "access", "policy", "apply", "--file", goPolicy},
		{"--json", "access", "magic-link", "add", "--id", "email", "--name", "Email", "--origin", "https://lb.example.com"},
		{"--json", "access", "cloudflare", "--team-domain", "team.cloudflareaccess.com", "--audience", "aud"},
		with("cloudflare", "--team-domain", "Bad", "--audience", "aud"),
		{"--json", "access", "cloudflare", "--team-domain", "team.cloudflareaccess.com", "--audience", "aud", "--origin", "http://x"},
		with("cloudflare", "--team-domain", "team.cloudflareaccess.com", "--audience", "aud", "--permissions", "bogus"),
		// With no --permissions a new provider grants nothing (DESIGN §13.6): this one is given a list.
		with("cloudflare", "--team-domain", "team.cloudflareaccess.com", "--audience", "aud", "--permissions", "inference:chat,models:read"),
		{"--json", "access", "show"},
		with("oidc", "add", "--id", "google", "--name", "Google", "--issuer", "https://accounts.google.com", "--client-id", "cid"),
		with("oidc", "add", "--id", "google", "--name", "Google", "--issuer", "https://accounts.google.com", "--client-id", "cid", "--client-secret-env", "MISSING_SECRET_VAR"),
		with("oidc", "add", "--id", "google", "--name", "Google", "--issuer", "https://accounts.google.com", "--client-id", "cid", "--client-secret-env", "CONTRACT_OIDC_SECRET", "--permissions", "models:read,inference:chat"),
		with("oidc", "add", "--id", "pub", "--name", "Public", "--issuer", "https://id.example.com", "--client-id", "pc", "--public-client"),
		{"--json", "access", "oidc", "list"},
		{"--json", "access", "show"},
		{"--json", "access", "email", "configure", "--host", "127.0.0.1", "--port", "1", "--security", "starttls", "--from", "lb@example.com", "--username", "u"},
		{"--json", "access", "email", "configure", "--host", "127.0.0.1", "--port", "1", "--security", "ssl", "--from", "lb@example.com"},
		{"--json", "access", "email", "configure", "--host", "127.0.0.1", "--port", "1", "--security", "starttls", "--from", "LB@example.com", "--username", "u", "--password-env", "CONTRACT_SMTP_PASSWORD"},
		{"--json", "access", "email", "show"},
		{"--json", "access", "magic-link", "add", "--id", "email", "--name", "Email", "--origin", "https://lb.example.com"},
		{"--json", "access", "magic-link", "add", "--id", "email2", "--name", "Email", "--origin", "https://lb.example.com"},
		{"--json", "access", "magic-link", "list"},
		{"--json", "access", "email", "disable"},
		{"--json", "access", "policy", "apply", "--file", notJSON},
		{"--json", "access", "policy", "apply", "--file", filepath.Join(dir, "missing.json")},
		{"--json", "access", "policy", "apply", "--file", badPolicy},
		{"--json", "access", "policy", "apply", "--file", goPolicy},
		{"--json", "access", "policy", "show"},
		{"--json", "access", "policy", "test", "--issuer", "https://accounts.google.com", "--subject", "s", "--email", "Owner@Example.com"},
		{"--json", "access", "policy", "test", "--issuer", "https://accounts.google.com", "--subject", "s"},
		{"--json", "access", "users", "invite", "--email", "Friend@Example.com", "--roles", "chat", "--issuer", vouching},
		{"--json", "access", "users", "invite", "--email", "friend@example.com", "--roles", "chat", "--issuer", vouching},
		{"--json", "access", "users", "invite", "--email", "x@example.com", "--roles", ""},
		{"--json", "access", "users", "invite", "--email", "x@example.com", "--roles", "missing", "--issuer", vouching},
		{"--json", "access", "users", "invite", "--email", "nope", "--roles", "chat"},
		{"--json", "access", "users", "roles", "--email", "friend@example.com", "--roles", "admin,chat"},
		{"--json", "access", "users", "roles", "--email", "friend@example.com", "--roles", ""},
		{"--json", "access", "users", "disable", "--email", "friend@example.com"},
		{"--json", "access", "users", "enable", "--email", "friend@example.com"},
		{"--json", "access", "users", "enable", "--email", "ghost@example.com"},
		{"--json", "access", "users", "list"},
		{"--json", "access", "users", "remove", "--email", "friend@example.com"},
		{"--json", "access", "policy", "clear"},
		{"--json", "access", "policy", "clear"},
		{"--json", "access", "oidc", "remove", "email"},
		{"--json", "access", "magic-link", "remove", "email"},
		{"--json", "access", "email", "disable"},
		{"--json", "access", "email", "disable"},
		{"--json", "access", "github", "list"},
		{"--json", "access", "oidc", "remove", "google"},
		{"--json", "access", "oidc", "remove", "pub"},
		{"--json", "access", "show"},
		{"--json", "access", "disable"},
		{"--json", "access", "oidc", "remove"},
	} {
		runCase(t, gr, args...)
	}
	// No provider and no policy are left: clearing succeeds and clears nothing (TS refused it).
	if goRes := runCLI(t, "go", gr, "--json", "access", "policy", "clear"); goRes.code != 0 || !strings.Contains(goRes.stdout, `"cleared":false`) {
		t.Errorf("go policy clear with no provider: want exit 0 and cleared false, got %d %s%s", goRes.code, goRes.stdout, goRes.stderr)
	}
}
