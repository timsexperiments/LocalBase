package contract

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/test/harness"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// A policy no one could manage access under is refused, in plain words on Go and in
// TS's wording on TS: a deliberate difference (DESIGN §13.6 item 12).
func TestPolicyLockoutMessage(t *testing.T) {
	impls := map[string]string{"go": "This policy would lock everyone out: at least one person it names must be able to manage access."}
	if harness.Impl() == "both" {
		if _, err := exec.LookPath("bun"); err == nil {
			impls["ts"] = "At least one reachable role must grant access:manage."
		}
	}
	for impl, want := range impls {
		t.Run(impl, func(t *testing.T) {
			dir := fsguard.ScratchDir(t)
			root := filepath.Join(dir, "root")
			if r := runCLI(t, impl, root, "--json", "init"); r.code != 0 {
				t.Fatalf("init: %s", r.stderr)
			}
			policy := filepath.Join(dir, "policy.json")
			fsguard.WriteFile(policy, []byte(`{"roles":[{"name":"chat","description":"Chat","permissions":["inference:chat"]}],`+
				`"bindings":[{"kind":"email","role":"chat","email":"owner@example.com"}],"defaultRole":null}`), 0o600)
			r := runCLI(t, impl, root, "access", "policy", "apply", "--file", policy)
			if r.code == 0 || !strings.Contains(r.stderr, want) {
				t.Errorf("text: %d %s", r.code, r.stderr)
			}
			j := runCLI(t, impl, root, "--json", "access", "policy", "apply", "--file", policy)
			if j.code == 0 || !strings.Contains(j.stdout, want) {
				t.Errorf("--json: %d %s", j.code, j.stdout)
			}
			// The refusal changed nothing.
			if s := runCLI(t, impl, root, "--json", "access", "policy", "show"); !strings.Contains(s.stdout, `"policy":null`) {
				t.Errorf("a refused policy was stored: %s", s.stdout)
			}
		})
	}
	// The API, which Admin shows: same status and code, plainer words on Go.
	for _, tg := range harness.Targets(t) {
		h := harness.Bearer(tg.Admin)
		h["Content-Type"] = "application/json"
		r := harness.Do(t, "POST", tg.BaseURL+"/_localbase/access-management", h, `{"action":"apply-policy","expectedPolicyRevision":null,"policy":`+
			`{"roles":[{"name":"chat","description":"Chat","permissions":["inference:chat"]}],"bindings":[{"kind":"email","role":"chat","email":"owner@example.com"}],"defaultRole":null}}`)
		want := map[string]string{"go": "This policy would lock everyone out", "ts": "The management request is invalid."}[tg.Name]
		if r.Status != 400 || !strings.Contains(string(r.Body), `"code":"validation_failed"`) || !strings.Contains(string(r.Body), want) {
			t.Errorf("%s API: %d %s", tg.Name, r.Status, r.Body)
		}
	}
}
