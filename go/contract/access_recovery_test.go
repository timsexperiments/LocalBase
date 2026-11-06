package contract

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The CLI is the break-glass path around the API's lockout rules (DESIGN
// §13.6): it applies a change that leaves nobody able to manage access, and
// says so on stderr; a change that keeps a manager stays quiet.
func TestCLIAccessRecovery(t *testing.T) {
	if *implFlag != "go" && *implFlag != "both" {
		t.Skip("runs the Go CLI")
	}
	root := filepath.Join(scratchDir(t), "root")
	policy := filepath.Join(scratchDir(t), "policy.json")
	mustWriteFile(policy, []byte(`{"roles":[{"name":"admin","description":"","permissions":["access:manage","access:read"]},`+
		`{"name":"chat","description":"","permissions":["inference:chat"]}],"bindings":[{"kind":"email","role":"admin","email":"owner@example.com"}],"defaultRole":null}`), 0o600)
	const warning = "Warning: nobody can manage access now"
	run := func(wantWarning bool, args ...string) {
		t.Helper()
		r := runCLI(t, "go", root, args...)
		if r.code != 0 {
			t.Fatalf("%v: exit %d %s%s", args, r.code, r.stdout, r.stderr)
		}
		if got := strings.Contains(r.stderr, warning); got != wantWarning {
			t.Fatalf("%v: warning %v, want %v\n%s", args, got, wantWarning, r.stderr)
		}
	}
	run(false, "--json", "init")
	run(false, "--json", "access", "cloudflare", "--team-domain", "team.cloudflareaccess.com", "--audience", "aud", "--origin", "https://lb.example.com")
	run(false, "--json", "access", "policy", "apply", "--file", policy)
	run(false, "--json", "access", "users", "invite", "--email", "owner@example.com", "--roles", "admin")
	// The only manager: the API would refuse; the CLI disables and warns.
	run(true, "--json", "access", "users", "disable", "--email", "owner@example.com")
	// Recovery: enabling restores a manager, quietly.
	run(false, "--json", "access", "users", "enable", "--email", "owner@example.com")
	// A key with access:manage is a manager too; revoking it while the owner
	// still manages is quiet.
	k := runCLI(t, "go", root, "--json", "keys", "create", "--name", "ops", "--scopes", "access:manage")
	m := regexp.MustCompile(`id=(key_[0-9a-f-]+)`).FindStringSubmatch(k.stderr)
	if m == nil {
		t.Fatalf("keys create: %s%s", k.stdout, k.stderr)
	}
	id := m[1]
	run(false, "--json", "keys", "revoke", id)
}
