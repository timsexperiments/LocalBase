package obs

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
)

// A bundle exists to be shared, so it doesn't say who it came from: not a person's name, in a path
// or in what they called a key, and not an address.
func TestABundleSaysNothingOfWhoItCameFrom(t *testing.T) {
	const name, address = "planted-person", "planted@example.com"
	home := "/Users/" + name + "/.local/share/local-base"
	for _, c := range []struct {
		what  string
		event map[string]any
	}{
		{"the root", map[string]any{"attributes": map[string]any{"root": home}}},
		{"a key's name, in capitals", map[string]any{"attributes": map[string]any{"AUTH_PRINCIPAL_NAME": name}}},
		{"a key's name", map[string]any{"attributes": map[string]any{"auth_principal_name": name}}},
		{"a key's id", map[string]any{"attributes": map[string]any{"auth_principal_id": name}}},
		{"who acted", map[string]any{"attributes": map[string]any{"actor_id": name}}},
		{"what they acted on", map[string]any{"attributes": map[string]any{"target_id": name}}},
		{"an address", map[string]any{"attributes": map[string]any{"email": address}}},
		{"a path in the message", map[string]any{"message": "Could not open " + home + "/gateway.db: no such file."}},
		{"a path in the error", map[string]any{"error": map[string]any{"message": "open " + home + ": denied"}}},
		{"a path in an attribute", map[string]any{"attributes": map[string]any{"detail": "stat " + home}}},
		{"a Linux home", map[string]any{"message": "open /home/" + name + "/x: denied"}},
		{"an address in the message", map[string]any{"message": "The mail server refused " + address + "."}},
		{"an address in the error", map[string]any{"error": map[string]any{"message": "550 no such user " + address}}},
		{"an address in an attribute", map[string]any{"attributes": map[string]any{"detail": "to " + address}}},
	} {
		base := map[string]any{"schemaVersion": 2, "id": "x", "timestamp": "t", "severity": "info", "eventName": "e",
			"category": "c", "component": "g", "runtime": "r", "message": "m"}
		for k, v := range c.event {
			base[k] = v
		}
		raw, err := json.Marshal(base)
		if err != nil {
			t.Fatal(err)
		}
		out, err := DiagnosticsEvent(raw)
		if err != nil {
			t.Fatal(err)
		}
		got := string(jsjson.Stringify(out))
		if strings.Contains(got, name) || strings.Contains(got, address) {
			t.Errorf("%s: %s", c.what, got)
		}
	}
}

// What makes a bundle useful stays: the event, the model, the status, and which file it was.
func TestABundleKeepsWhatHelps(t *testing.T) {
	raw := []byte(`{"schemaVersion":2,"id":"x","timestamp":"t","severity":"error","eventName":"runtime.exited","category":"c","component":"g","runtime":"r",
		"message":"Could not open /Users/someone/.local/share/local-base/gateway.db: no such file.",
		"error":{"message":"exit status 1"},
		"attributes":{"model_id":"qwen","status":503,"auth_outcome":"authenticated","auth_source":"stored_key","detail":"/opt/local-base/x"}}`)
	out, err := DiagnosticsEvent(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := string(jsjson.Stringify(out))
	for _, want := range []string{"runtime.exited", "qwen", "503", "authenticated", "stored_key", "gateway.db: no such file", "exit status 1", "/opt/local-base/x"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is gone: %s", want, got)
		}
	}
}
