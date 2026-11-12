package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `access policy apply` refuses a new email or domain binding that names a provider which isn't
// configured, as the gateway does, in issuerUnknown's words, as an input error. A binding the
// stored policy already has may stay, even once its provider is gone.
func TestPolicyApplyRefusesANewBindingOnAnUnknownProvider(t *testing.T) {
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
	const a, b, c = "https://a.example.com", "https://b.example.com", "https://c.example.com"
	for _, p := range [][2]string{{"a", a}, {"b", b}} {
		ok("access", "oidc", "add", "--id", p[0], "--name", "P", "--issuer", p[1], "--client-id", "x",
			"--public-client", "--origin", "https://lb.example.com")
	}
	dir := t.TempDir()
	apply := func(bindings string) (int, string) {
		t.Helper()
		file := filepath.Join(dir, "policy.json")
		policy := `{"roles":[{"name":"admin","description":"Admin","permissions":["access:manage","access:read"]}],` +
			`"bindings":[` + bindings + `],"defaultRole":null}`
		if err := os.WriteFile(file, []byte(policy), 0o600); err != nil {
			t.Fatal(err)
		}
		return run("access", "policy", "apply", "--file", file)
	}
	binding := func(email, issuer string) string {
		return `{"kind":"email","role":"admin","email":"` + email + `","issuer":"` + issuer + `"}`
	}
	shown := func() string { t.Helper(); return ok("--json", "access", "policy", "show") }

	// A new binding on a provider that isn't configured: refused, and nothing is stored.
	before := shown()
	code, out := apply(binding("owner@example.com", c))
	if want := c + " is not a configured sign-in provider; these are: " + a + ", " + b + "."; code != 2 || !strings.Contains(out, want) {
		t.Errorf("a new binding on %s: exit %d, %s; want exit 2 and %q", c, code, out, want)
	}
	if after := shown(); after != before {
		t.Errorf("a refused apply changed the policy:\nwas %s\nnow %s", before, after)
	}

	// On a configured provider it is applied; once that provider is removed, the same binding may
	// stay on a re-apply, but a new one naming it is refused.
	if code, out := apply(binding("owner@example.com", b)); code != 0 {
		t.Fatalf("a binding on %s: exit %d, %s", b, code, out)
	}
	ok("access", "oidc", "remove", "b")
	if code, out := apply(binding("owner@example.com", b)); code != 0 {
		t.Errorf("re-applying a binding whose provider is gone: exit %d, %s", code, out)
	}
	code, out = apply(binding("owner@example.com", b) + "," + binding("new@example.com", b))
	if want := b + " is not a configured sign-in provider; these are: " + a + "."; code != 2 || !strings.Contains(out, want) {
		t.Errorf("a new binding on the removed %s: exit %d, %s; want exit 2 and %q", b, code, out, want)
	}

	// Recorded as it stands, not changed here: with several providers, a binding that names none is
	// refused (issuer-required) with exit 1, through accessError.
	ok("access", "oidc", "add", "--id", "b", "--name", "P", "--issuer", b, "--client-id", "x",
		"--public-client", "--origin", "https://lb.example.com")
	code, out = apply(`{"kind":"email","role":"admin","email":"owner@example.com"}`)
	if code != 1 || !strings.Contains(out, "must name the sign-in provider that vouches for it") {
		t.Errorf("a binding with no provider among several: exit %d, %s", code, out)
	}
}

// `access policy clear` needs no provider, as on the gateway: with no policy it says so, and a
// policy left behind when browser access was disabled can still be cleared.
func TestPolicyClearNeedsNoProvider(t *testing.T) {
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
	audits := func() int { t.Helper(); return len(auditLines(t, root)) }

	before := audits()
	if out := ok("--json", "access", "policy", "clear"); !strings.Contains(out, `"cleared":false`) {
		t.Errorf("clear with no provider and no policy: %s", out)
	}
	if out := ok("access", "policy", "clear"); !strings.Contains(out, "Browser access policy was already clear.") {
		t.Errorf("clear with no provider and no policy: %s", out)
	}
	if n := audits(); n != before {
		t.Errorf("clearing nothing wrote %d audit lines", n-before)
	}

	ok("access", "oidc", "add", "--id", "a", "--name", "P", "--issuer", "https://a.example.com", "--client-id", "x",
		"--public-client", "--origin", "https://lb.example.com")
	ok("access", "policy", "starter", "--email", "owner@example.com")
	ok("access", "disable")
	before = audits()
	if out := ok("--json", "access", "policy", "clear"); !strings.Contains(out, `"cleared":true`) {
		t.Errorf("clear a policy left after disabling browser access: %s", out)
	}
	if n := audits(); n != before+1 {
		t.Errorf("clearing a policy wrote %d audit lines, want 1", n-before)
	}
	if out := ok("--json", "access", "policy", "show"); !strings.Contains(out, `"policy":null`) {
		t.Errorf("the policy is still there: %s", out)
	}
}
