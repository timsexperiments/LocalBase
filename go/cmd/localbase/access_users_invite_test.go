package main

import (
	"os"
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

// `users invite --send-email` needs email delivery: with none it's bad input (exit 2); with a file
// that can't be read it says so in LoadEmail's words (exit 1), and invites nobody either way.
func TestAnInviteThatMustSendNeedsReadableEmail(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	root := filepath.Join(t.TempDir(), "root")
	run := func(args ...string) (int, string) {
		t.Helper()
		return runApp(t, append([]string{"--root", root}, args...)...)
	}
	ok := func(args ...string) {
		t.Helper()
		if code, out := run(args...); code != 0 {
			t.Fatalf("%s: exit %d, %s", strings.Join(args, " "), code, out)
		}
	}
	ok("configure", "--defaults", "--no-create-key")
	ok("access", "oidc", "add", "--id", "a", "--name", "P", "--issuer", "https://a.example.com", "--client-id", "x",
		"--public-client", "--origin", "https://lb.example.com")
	ok("access", "policy", "starter", "--email", "owner@example.com")
	invite := []string{"access", "users", "invite", "--email", "one@example.com", "--roles", "member", "--send-email"}
	if code, out := run(invite...); code != 2 || !strings.Contains(out, "Configure email delivery before sending an invitation email.") {
		t.Errorf("with no email delivery: exit %d, %s", code, out)
	}
	if err := os.WriteFile(filepath.Join(root, "email-delivery.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := run(invite...); code != 1 || !strings.Contains(out, "Invalid email-delivery.json. Configure email delivery again with the LocalBase CLI.") {
		t.Errorf("with an unreadable file: exit %d, %s", code, out)
	}
	if _, out := run("--json", "access", "users", "list"); strings.Contains(out, "one@example.com") {
		t.Errorf("someone was invited: %s", out)
	}
}
