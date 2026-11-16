package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The email commands: configure replaces a file it can't read (it never reads the current one),
// and disable is refused, writing nothing, while a magic-link registration needs email delivery.
func TestEmailCommandsKeepTheirRules(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	t.Setenv("EMAIL_TEST_SMTP_PASSWORD", "pw")
	root := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
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
	file := filepath.Join(root, "email-delivery.json")
	saved := func() string {
		b, err := os.ReadFile(file)
		if os.IsNotExist(err) {
			return "(none)"
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	if err := os.WriteFile(file, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	ok("access", "email", "configure", "--host", "smtp.example", "--port", "587", "--security", "starttls",
		"--username", "user", "--password-env", "EMAIL_TEST_SMTP_PASSWORD", "--from", "sender@example.com")
	if got := saved(); !strings.Contains(got, `"host": "smtp.example"`) {
		t.Fatalf("configure over an unreadable file saved %s", got)
	}

	ok("access", "magic-link", "add", "--id", "email", "--name", "Email", "--origin", "https://lb.example.com")
	before := saved()
	code, out := run("access", "email", "disable")
	if code != 2 || !strings.Contains(out, "Remove the magic-link registration before disabling email delivery.") {
		t.Errorf("disable with a magic link: exit %d, %s", code, out)
	}
	if after := saved(); after != before {
		t.Errorf("a refused disable changed the file:\nwas %s\nnow %s", before, after)
	}

	ok("access", "magic-link", "remove", "email")
	if out := ok("access", "email", "disable"); !strings.Contains(out, "Disabled email delivery.") {
		t.Errorf("disable: %s", out)
	}
	if got := saved(); got != "(none)" {
		t.Errorf("email-delivery.json is still there: %s", got)
	}
	if out := ok("access", "email", "disable"); !strings.Contains(out, "Email delivery was already disabled.") {
		t.Errorf("disable again: %s", out)
	}
}
