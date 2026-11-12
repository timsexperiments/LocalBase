package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
)

// An email-delivery.json that can't be read is refused as what it is (409
// email_delivery_unreadable, with LoadEmail's words), not as "not configured", by every action that
// needs email delivery; with no file at all it is still email_delivery_not_configured.
func TestAnUnreadableEmailFileIsSaidAsSuch(t *testing.T) {
	f := newAuditFixture(t)
	send := func(body string) (int, string) {
		t.Helper()
		status, out := f.send(f.key, "POST", "/_localbase/access-management", "application/json", []byte(body))
		return status, string(out)
	}
	if status, out := send(gatewaytest.ConfigureCloudflare); status != 200 {
		t.Fatalf("configure: %d %s", status, out)
	}
	if status, out := send(gatewaytest.PolicyRequest("null", gatewaytest.ChatRole+","+gatewaytest.AdminRole,
		`{"kind":"email","role":"admin","email":"owner@example.test"}`, "null")); status != 200 {
		t.Fatalf("policy: %d %s", status, out)
	}
	actions := map[string]string{
		"upsert-magic-link":   `{"action":"upsert-magic-link","registration":{"kind":"magic-link","id":"email","name":"Email"},"origin":"https://lb.example.test","permissions":["inference:chat"]}`,
		"invite-user":         `{"action":"invite-user","email":"friend@example.test","roles":["chat"],"sendEmail":true}`,
		"test-email-delivery": `{"action":"test-email-delivery","to":"friend@example.test"}`,
	}
	for name, body := range actions {
		if status, out := send(body); status != 409 || !strings.Contains(out, `"email_delivery_not_configured"`) {
			t.Errorf("%s with no file: %d %s", name, status, out)
		}
	}
	file := filepath.Join(f.srv.cfg.Root.Dir, "email-delivery.json")
	if err := os.WriteFile(file, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	const why = "Invalid email-delivery.json. Configure email delivery again with the LocalBase CLI."
	for name, body := range actions {
		if status, out := send(body); status != 409 || !strings.Contains(out, `"code":"email_delivery_unreadable"`) || !strings.Contains(out, why) {
			t.Errorf("%s with an unreadable file: %d %s", name, status, out)
		}
	}
}
