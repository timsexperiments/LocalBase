package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// An existing root has email bindings and invitations that don't say which sign-in provider
// vouches for them. With one provider, the first start names it and the owner stays signed
// in. With several, the gateway still starts: nobody guesses, those rows grant nothing until
// someone chooses, and the log, doctor and status say how. The host commands keep working.
func TestServeNamesTheProviderOnOlderBindings(t *testing.T) {
	if *implFlag != "go" && *implFlag != "both" {
		t.Skip("runs the Go CLI")
	}
	tmp := scratchDir(t)
	e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp}
	must := func(args ...string) cliResult {
		t.Helper()
		r := e.cli(nil, args...)
		if r.code != 0 {
			t.Fatalf("%v: exit %d %s%s", args, r.code, r.stdout, r.stderr)
		}
		return r
	}
	provider := func(id, issuer string) {
		t.Helper()
		must("--json", "access", "oidc", "add", "--id", id, "--name", id, "--issuer", issuer, "--client-id", "x", "--public-client",
			"--origin", "https://lb.example.test", "--permissions", "inference:chat")
	}
	// What a version that didn't know about providers leaves behind.
	forget := func() {
		t.Helper()
		st, err := store.Open(filepath.Join(e.root, "local-base.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if _, err := st.DB.Exec(`DELETE FROM go_binding_issuers`); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.Exec(`DELETE FROM go_invitation_issuers`); err != nil {
			t.Fatal(err)
		}
	}
	const a, b = "https://a.example.test", "https://b.example.test"
	matched := func(issuer string) []string {
		t.Helper()
		r := must("--json", "access", "policy", "test", "--issuer", issuer, "--subject", "s", "--email", "owner@example.test")
		var out struct {
			Data struct {
				MatchedRoles []string `json:"matchedRoles"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
			t.Fatalf("%v: %s", err, r.stdout)
		}
		return out.Data.MatchedRoles
	}
	policy := filepath.Join(tmp, "policy.json")
	mustWriteFile(policy, []byte(`{"roles":[{"name":"owner","description":"","permissions":["access:manage","access:read"]},`+
		`{"name":"chat","description":"","permissions":["inference:chat"]}],`+
		`"bindings":[{"kind":"email","role":"owner","email":"owner@example.test"}],"defaultRole":null}`), 0o600)

	must("--json", "init")
	provider("a", a)
	must("--json", "access", "policy", "apply", "--file", policy)
	must("--json", "access", "users", "invite", "--email", "pending@example.test", "--roles", "chat")
	if got := matched(a); len(got) != 1 {
		t.Fatalf("the CLI fills in the only provider: %v", got)
	}

	// One provider: the first start names it, and only it.
	forget()
	if got := matched(a); len(got) != 0 {
		t.Fatalf("an older binding before the first start: %v", got)
	}
	e.serve()
	e.stop()
	if got := matched(a); len(got) != 1 || got[0] != "owner" {
		t.Errorf("the owner after the first start: %v", got)
	}
	if got := matched(b); len(got) != 0 {
		t.Errorf("another provider after the first start: %v", got)
	}
	log, err := os.ReadFile(filepath.Join(e.root, "logs", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "auth.issuers-adopted") {
		t.Errorf("the adoption is not on record")
	}

	// Several providers: it starts, names nothing, and says so once per group.
	provider("b", b)
	forget()
	mustWriteFile(filepath.Join(e.root, "logs", "events.jsonl"), nil, 0o600)
	e.serve()
	e.stop()
	if got := matched(a); len(got) != 0 {
		t.Errorf("a start with several providers named one: %v", got)
	}
	if got := matched(b); len(got) != 0 {
		t.Errorf("a start with several providers named one: %v", got)
	}
	log, err = os.ReadFile(filepath.Join(e.root, "logs", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(log), "auth.bindings-unbound"); n != 2 {
		t.Errorf("%d warnings, want one for the email binding and one for the invitation:\n%s", n, log)
	}
	if !strings.Contains(string(log), "local-base access bindings adopt --issuer") {
		t.Errorf("the warning doesn't say what to do:\n%s", log)
	}
	if strings.Contains(string(log), "owner@example.test") || strings.Contains(string(log), "pending@example.test") {
		t.Errorf("the warning names an address:\n%s", log)
	}
	const notice = "2 bindings and invitations are not bound to a sign-in provider"
	for _, command := range []string{"doctor", "status"} {
		if r := must(command); !strings.Contains(r.stdout+r.stderr, notice) || !strings.Contains(r.stdout+r.stderr, "local-base access bindings adopt --issuer") {
			t.Errorf("%s doesn't say so:\n%s%s", command, r.stdout, r.stderr)
		}
	}
	// Without sign-in nothing depends on it, so that mode says nothing about it.
	mustWriteFile(filepath.Join(e.root, "logs", "events.jsonl"), nil, 0o600)
	e.serve("--no-auth")
	e.stop()
	log, err = os.ReadFile(filepath.Join(e.root, "logs", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), "auth.bindings-unbound") {
		t.Errorf("--no-auth warned about sign-in:\n%s", log)
	}

	// The way on: one command, for a provider that exists.
	if r := e.cli(nil, "--json", "access", "bindings", "adopt", "--issuer", "https://c.example.test"); r.code == 0 {
		t.Errorf("adopting a provider that isn't configured: %s", r.stdout)
	}
	must("--json", "access", "bindings", "adopt", "--issuer", b)
	if got := matched(b); len(got) != 1 {
		t.Errorf("after choosing: %v", got)
	}
	if r := must("doctor"); strings.Contains(r.stdout+r.stderr, "not bound to a sign-in provider") {
		t.Errorf("doctor after choosing:\n%s%s", r.stdout, r.stderr)
	}
	if got := matched(a); len(got) != 0 {
		t.Errorf("the provider not chosen: %v", got)
	}
	e.serve()
	e.stop()

	// With several, the CLI asks too, and takes an answer.
	if r := e.cli(nil, "--json", "access", "policy", "apply", "--file", policy); r.code == 0 || !strings.Contains(r.stderr+r.stdout, "several are configured") {
		t.Errorf("a policy with no provider named: %d %s%s", r.code, r.stdout, r.stderr)
	}
	if r := e.cli(nil, "--json", "access", "users", "invite", "--email", "next@example.test", "--roles", "chat"); r.code == 0 || !strings.Contains(r.stderr+r.stdout, "several are configured") {
		t.Errorf("an invitation with no provider named: %d %s%s", r.code, r.stdout, r.stderr)
	}
	r := must("--json", "access", "users", "invite", "--email", "next@example.test", "--roles", "chat", "--issuer", b)
	if !strings.Contains(r.stdout, `"issuer":"`+b+`"`) {
		t.Errorf("the invitation: %s", r.stdout)
	}

	// With no provider at all, nobody can sign in and there is nothing to choose: quiet.
	must("--json", "access", "disable")
	forget()
	mustWriteFile(filepath.Join(e.root, "logs", "events.jsonl"), nil, 0o600)
	e.serve()
	e.stop()
	log, err = os.ReadFile(filepath.Join(e.root, "logs", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), "auth.bindings-unbound") {
		t.Errorf("a warning with no provider to choose:\n%s", log)
	}
	if r := must("doctor"); strings.Contains(r.stdout+r.stderr, "not bound to a sign-in provider") {
		t.Errorf("doctor with no provider to choose:\n%s%s", r.stdout, r.stderr)
	}
}
