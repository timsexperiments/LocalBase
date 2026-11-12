package gateway

import (
	"strings"
	"testing"
)

// A change that changes nothing writes no audit line; a refusal is always audited, and a real
// change still writes its line.
func TestAnUnchangedAccessChangeWritesNoAuditLine(t *testing.T) {
	f := newAuditFixture(t)
	access := func(body string, want int) []map[string]any {
		t.Helper()
		from := len(f.logs.String())
		if status, out := f.send(f.key, "POST", "/_localbase/access-management", "application/json", []byte(body)); status != want {
			t.Fatalf("%s: %d %s, want %d", body, status, out, want)
		}
		return f.auditEvents(from)
	}
	lines := func(events []map[string]any) string {
		var out []string
		for _, ev := range events {
			out = append(out, ev["event"].(string)+" "+ev["action"].(string))
		}
		return strings.Join(out, ", ")
	}

	// Tier 1: turning off what is already off.
	for _, body := range []string{`{"action":"disable"}`, `{"action":"disable-email-delivery"}`} {
		if got := access(body, 200); len(got) != 0 {
			t.Errorf("%s, with nothing to turn off: %s", body, lines(got))
		}
	}
	// A real change still writes its line, and so does turning it off again.
	cloudflare := `{"action":"configure-cloudflare","provider":{"kind":"cloudflare-access","teamDomain":"team.cloudflareaccess.com","audience":"aud"},"origin":"https://lb.example.test","permissions":[]}`
	if got := access(cloudflare, 200); lines(got) != "audit.access-changed configure-cloudflare" {
		t.Errorf("configure: %s", lines(got))
	}
	if got := access(`{"action":"disable"}`, 200); lines(got) != "audit.access-changed disable" {
		t.Errorf("disable: %s", lines(got))
	}
	// A refusal is audited whatever it would have changed.
	if got := access(`{"action":"remove-registration","registrationId":"nobody"}`, 404); lines(got) != "audit.access-refused remove-registration" {
		t.Errorf("a refusal: %s", lines(got))
	}
}
