package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// `access policy test` says what the policy gives an identity: refused with no provider (exit 2),
// the provider-wide permissions with no policy, and the matched roles under one.
func TestPolicyTestCommand(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	root := filepath.Join(t.TempDir(), "root")
	run := func(args ...string) (int, string) {
		t.Helper()
		return runApp(t, append([]string{"--root", root}, args...)...)
	}
	ok := func(args ...string) string {
		t.Helper()
		code, out := run(args...)
		if code != 0 {
			t.Fatalf("%s: exit %d, %s", strings.Join(args, " "), code, out)
		}
		return out
	}
	ok("configure", "--defaults", "--no-create-key")
	const a = "https://a.example.com"
	test := []string{"--json", "access", "policy", "test", "--issuer", a, "--subject", "s", "--email", "owner@example.com"}

	if code, out := run(test...); code != 2 || !strings.Contains(out, "Configure a browser identity provider first.") {
		t.Errorf("no provider: exit %d, %s", code, out)
	}
	ok("access", "oidc", "add", "--id", "a", "--name", "P", "--issuer", a, "--client-id", "x", "--public-client",
		"--origin", "https://lb.example.com", "--permissions", "models:read")
	if out := ok(test...); !strings.Contains(out, `"matchedRoles":[]`) || !strings.Contains(out, `"permissions":["models:read"]`) ||
		!strings.Contains(out, `"policyConfigured":false`) {
		t.Errorf("no policy: %s", out)
	}
	ok("access", "policy", "starter", "--email", "owner@example.com")
	if out := ok(test...); !strings.Contains(out, `"matchedRoles":["admin"]`) || !strings.Contains(out, `"policyConfigured":true`) {
		t.Errorf("the starter policy: %s", out)
	}
	if out := ok("access", "policy", "test", "--issuer", a, "--subject", "x"); !strings.Contains(out, "Matched 0 roles and 0 permissions.") {
		t.Errorf("someone the policy doesn't name: %s", out)
	}
}
