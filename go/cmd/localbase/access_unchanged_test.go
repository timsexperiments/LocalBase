package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A command that saves what is already saved writes no audit line, judged in the form the save
// keeps; a real change writes its line. (The gateway's TestSavingWhatIsSavedWritesNoAuditLine is
// the same rule.)
func TestSavingWhatIsSavedWritesNoAuditLine(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	t.Setenv("UNCHANGED_TEST_SMTP_PASSWORD", "pw")
	root := filepath.Join(t.TempDir(), "root")
	run := func(args ...string) {
		t.Helper()
		if code, out := runApp(t, append([]string{"--root", root}, args...)...); code != 0 {
			t.Fatalf("%s: exit %d, %s", strings.Join(args, " "), code, out)
		}
	}
	run("configure", "--defaults", "--no-create-key")
	lines := func(args ...string) int {
		t.Helper()
		before := len(auditLines(t, root))
		run(args...)
		return len(auditLines(t, root)) - before
	}
	origin := []string{"--origin", "https://lb.example.com"}
	oidc := func(name, perms string) []string {
		return append([]string{"access", "oidc", "add", "--id", "a", "--name", name, "--issuer", "https://a.example.com",
			"--client-id", "x", "--public-client", "--permissions", perms}, origin...)
	}
	email := func(host string) []string {
		return []string{"access", "email", "configure", "--host", host, "--port", "587", "--security", "starttls",
			"--username", "user", "--password-env", "UNCHANGED_TEST_SMTP_PASSWORD", "--from", "sender@example.com"}
	}
	dir := t.TempDir()
	policy := func(roles, bindings string) []string {
		file := filepath.Join(dir, "policy.json")
		if err := os.WriteFile(file, []byte(`{"roles":[`+roles+`],"bindings":[`+bindings+`],"defaultRole":null}`), 0o600); err != nil {
			t.Fatal(err)
		}
		return []string{"access", "policy", "apply", "--file", file}
	}
	const chat = `{"name":"chat","description":"Chat","permissions":["inference:chat"]}`
	const admin = `{"name":"admin","description":"Admin","permissions":["access:manage","access:read"]}`
	const owner = `{"kind":"email","role":"admin","email":"owner@example.com"}`
	const domain = `{"kind":"email-domain","role":"chat","domain":"example.com"}`
	cloudflare := func(perms string) []string {
		return append([]string{"access", "cloudflare", "--team-domain", "team.cloudflareaccess.com", "--audience", "aud", "--permissions", perms}, origin...)
	}

	for _, step := range []struct {
		name  string
		args  func() []string
		lines int
	}{
		{"a registration", func() []string { return oidc("A", "inference:chat,models:read") }, 1},
		{"the same again", func() []string { return oidc("A", "inference:chat,models:read") }, 0},
		{"its permissions in another order", func() []string { return oidc("A", "models:read,inference:chat") }, 0},
		{"its name changed", func() []string { return oidc("Renamed", "inference:chat,models:read") }, 1},
		{"email delivery", func() []string { return email("smtp.example.com") }, 1},
		{"the same again", func() []string { return email("smtp.example.com") }, 0},
		{"another host", func() []string { return email("mail.example.com") }, 1},
		{"a policy", func() []string { return policy(chat+","+admin, owner+","+domain) }, 1},
		{"the same again", func() []string { return policy(chat+","+admin, owner+","+domain) }, 0},
		{"its roles and bindings in another order", func() []string { return policy(admin+","+chat, domain+","+owner) }, 0},
		{"a binding fewer", func() []string { return policy(chat+","+admin, owner) }, 1},
		{"Cloudflare", func() []string { return cloudflare("inference:chat") }, 1},
		{"the same again", func() []string { return cloudflare("inference:chat") }, 0},
		{"another permission", func() []string { return cloudflare("inference:chat,models:read") }, 1},
	} {
		if got := lines(step.args()...); got != step.lines {
			t.Errorf("%s: %d audit lines, want %d", step.name, got, step.lines)
		}
	}
}
