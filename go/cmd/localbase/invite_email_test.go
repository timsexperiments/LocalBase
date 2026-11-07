package main

import (
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// invite --send-email says so when the email wasn't sent; the invite itself still stands (exit 0).
// Before, it printed the same line as an invite with no email asked for (review 01-M9).
func TestAnInviteWhoseEmailFailsSaysSo(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	root := filepath.Join(t.TempDir(), "root")
	run := func(args ...string) (int, string) {
		t.Helper()
		return runApp(t, append([]string{"--root", root}, args...)...)
	}
	if code, out := run("configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	// An SMTP server that isn't there: every send fails.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	ln.Close()
	const issuer, origin, invited = "https://identity.example", "https://lb.example", "invited@example.com"
	t.Setenv("INVITE_TEST_CLIENT_SECRET", "a-client-secret-for-the-test")
	t.Setenv("INVITE_TEST_SMTP_PASSWORD", "an-smtp-password-for-the-test")
	for _, args := range [][]string{
		{"access", "oidc", "add", "--id", "idp", "--name", "Provider", "--issuer", issuer, "--client-id", "client", "--client-secret-env", "INVITE_TEST_CLIENT_SECRET", "--origin", origin},
		{"access", "email", "configure", "--host", "127.0.0.1", "--port", port, "--security", "starttls", "--username", "user", "--password-env", "INVITE_TEST_SMTP_PASSWORD", "--from", "sender@example.com"},
		{"access", "policy", "starter", "--email", "owner@example.com", "--issuer", issuer},
	} {
		if code, out := run(args...); code != 0 {
			t.Fatalf("%v: %s", args, out)
		}
	}
	code, out := run("access", "users", "invite", "--email", invited, "--roles", "admin", "--issuer", issuer, "--send-email")
	if code != 0 {
		t.Fatalf("the invite failed: %s", out)
	}
	want := "Invited " + invited + ", but the email could not be sent. Sign in at "
	if !strings.Contains(out, want) {
		t.Errorf("no word of the failed email:\n%s", out)
	}
	if strings.Contains(out, "an-smtp-password-for-the-test") {
		t.Errorf("the SMTP password reached the output")
	}
}
