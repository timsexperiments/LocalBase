package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

// The provider commands say why a change was refused in their own words, which the plan cores in
// internal/auth leave to them, and a refused change writes nothing. (The contract suite's
// TestCLIAccess pins them, as R3a recorded them where they matched TS's.)
func TestProviderCommandsSayWhyAChangeWasRefused(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	t.Setenv("PROVIDERS_TEST_SMTP_PASSWORD", "pw")
	root := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	origin := []string{"--origin", "https://lb.example.com"}
	run := func(args ...string) (int, string) {
		t.Helper()
		return runApp(t, append([]string{"--root", root}, args...)...)
	}
	saved := func() string {
		b, err := os.ReadFile(auth.AccessPath(root))
		if os.IsNotExist(err) {
			return "(none)"
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	refused := func(want string, args ...string) {
		t.Helper()
		before := saved()
		code, out := run(args...)
		if code != 2 || !strings.Contains(out, want) {
			t.Errorf("%s: exit %d, %q; want exit 2 and %q", strings.Join(args, " "), code, out, want)
		}
		if after := saved(); after != before {
			t.Errorf("%s wrote ui-access.json:\nwas %s\nnow %s", strings.Join(args, " "), before, after)
		}
	}
	ok := func(args ...string) string {
		t.Helper()
		code, out := run(args...)
		if code != 0 {
			t.Fatalf("%s: exit %d, %s", strings.Join(args, " "), code, out)
		}
		return out
	}

	refused("Configure email delivery before enabling magic-link authentication.",
		append([]string{"access", "magic-link", "add", "--id", "email", "--name", "Email"}, origin...)...)
	refused("provider: invalid Cloudflare Access settings",
		append([]string{"access", "cloudflare", "--team-domain", "Bad", "--audience", "aud"}, origin...)...)
	refused("Expected an HTTPS OIDC issuer without credentials, query, or fragment.",
		append([]string{"access", "oidc", "add", "--id", "idp", "--name", "Provider", "--issuer", "http://id.example.com",
			"--client-id", "c", "--public-client"}, origin...)...)

	// An email delivery file that can't be read is said as such, in LoadEmail's words, with exit
	// 1: it's the machine's state, not bad input (the operator's ruling with the gateway's
	// email_delivery_unreadable; before, it counted as none, exit 2). Nothing is written.
	if err := os.WriteFile(filepath.Join(root, "email-delivery.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	{
		before := saved()
		args := append([]string{"access", "magic-link", "add", "--id", "email", "--name", "Email"}, origin...)
		const want = "Invalid email-delivery.json. Configure email delivery again with the LocalBase CLI."
		if code, out := run(args...); code != 1 || !strings.Contains(out, want) {
			t.Errorf("magic-link add with an unreadable email file: exit %d, %q; want exit 1 and %q", code, out, want)
		}
		if after := saved(); after != before {
			t.Errorf("a refused magic-link add wrote ui-access.json:\nwas %s\nnow %s", before, after)
		}
	}

	ok("access", "email", "configure", "--host", "smtp.example", "--port", "587", "--security", "starttls",
		"--username", "user", "--password-env", "PROVIDERS_TEST_SMTP_PASSWORD", "--from", "sender@example.com")
	ok(append([]string{"access", "magic-link", "add", "--id", "email", "--name", "Email"}, origin...)...)
	refused("Remove magic-link registration email before adding another.",
		append([]string{"access", "magic-link", "add", "--id", "email2", "--name", "Email"}, origin...)...)
	// The remove commands look only at registrations of their own kind.
	refused("OpenID Connect registration email not found.", "access", "oidc", "remove", "email")
	refused("Magic-link registration other not found.", "access", "magic-link", "remove", "other")

	// Without --permissions a provider command keeps the current set; Cloudflare replaces the
	// registrations and keeps them too.
	ok(append([]string{"access", "oidc", "add", "--id", "idp", "--name", "Provider", "--issuer", "https://id.example.com",
		"--client-id", "c", "--public-client", "--permissions", "models:read"}, origin...)...)
	ok(append([]string{"access", "cloudflare", "--team-domain", "team.cloudflareaccess.com", "--audience", "aud"}, origin...)...)
	if got := saved(); !strings.Contains(got, `"cloudflare-access"`) || !strings.Contains(got, `"permissions": [
    "models:read"
  ]`) {
		t.Errorf("cloudflare without --permissions saved %s", got)
	}
	ok(append([]string{"access", "magic-link", "add", "--id", "email", "--name", "Email"}, origin...)...)
	if got := saved(); !strings.Contains(got, `"magic-link"`) || strings.Contains(got, `"cloudflare-access"`) || !strings.Contains(got, `"permissions": [
    "models:read"
  ]`) {
		t.Errorf("magic-link add without --permissions saved %s", got)
	}

	// Removing the last registration turns browser access off.
	if out := ok("access", "magic-link", "remove", "email"); !strings.Contains(out, "Removed email and disabled browser access.") {
		t.Errorf("remove the last: %s", out)
	}
	if got := saved(); got != "(none)" {
		t.Errorf("ui-access.json is still there: %s", got)
	}
}
