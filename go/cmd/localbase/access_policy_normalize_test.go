package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `access policy apply` normalizes the policy itself, so binding its issuers sees it as it will
// be stored: with two providers, the refusal for an address with no provider names the address
// in lower case, as the policy would hold it.
func TestPolicyApplyBindsTheNormalizedPolicy(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	root := filepath.Join(t.TempDir(), "root")
	run := func(args ...string) (int, string) {
		t.Helper()
		return runApp(t, append([]string{"--root", root}, args...)...)
	}
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	for _, p := range [][2]string{{"a", "https://a.example.com"}, {"b", "https://b.example.com"}} {
		if code, out := run("access", "oidc", "add", "--id", p[0], "--name", "P", "--issuer", p[1], "--client-id", "c",
			"--public-client", "--origin", "https://lb.example.com"); code != 0 {
			t.Fatalf("oidc add: %s", out)
		}
	}
	file := filepath.Join(t.TempDir(), "policy.json")
	policy := `{"roles":[{"name":"admin","description":"Admin","permissions":["access:manage","access:read"]}],` +
		`"bindings":[{"kind":"email","role":"admin","email":"Owner@Example.COM"}],"defaultRole":null}`
	if err := os.WriteFile(file, []byte(policy), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := run("access", "policy", "apply", "--file", file)
	if code == 0 || !strings.Contains(out, "A binding for owner@example.com must name the sign-in provider") {
		t.Errorf("exit %d: %s", code, out)
	}
}
