package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// An invitation gives the browser origin's sign-in page, in the JSON and in the line it prints.
func TestAnInviteGivesTheSignInPage(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	root := filepath.Join(t.TempDir(), "root")
	run := func(args ...string) string {
		t.Helper()
		code, out := runApp(t, append([]string{"--root", root}, args...)...)
		if code != 0 {
			t.Fatalf("%s: exit %d, %s", strings.Join(args, " "), code, out)
		}
		return out
	}
	run("configure", "--defaults", "--no-create-key")
	const a, origin = "https://a.example.com", "https://lb.example.com"
	run("access", "oidc", "add", "--id", "a", "--name", "P", "--issuer", a, "--client-id", "x", "--public-client", "--origin", origin)
	run("access", "policy", "starter", "--email", "owner@example.com")
	if out := run("--json", "access", "users", "invite", "--email", "one@example.com", "--roles", "member"); !strings.Contains(out, `"signInUrl":"`+origin+`/app/login"`) ||
		!strings.Contains(out, `"emailDelivered":false`) {
		t.Errorf("invite: %s", out)
	}
	if out := run("access", "users", "invite", "--email", "two@example.com", "--roles", "member"); !strings.Contains(out, "Invited two@example.com. Sign in at "+origin+"/app/login") {
		t.Errorf("invite: %s", out)
	}
}
