package auth

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// What a key shows: its metadata, scopes as a list even when none, the expiry and revocation only
// when set, and never its hash.
func TestPublicKey(t *testing.T) {
	when := "2026-01-02T00:00:00.000Z"
	k := &store.APIKey{ID: "key_1", Name: "n", Prefix: "lb_abc", KeyHash: "secret-hash", CreatedAt: "c", LastRotatedAt: "r"}
	b, _ := json.Marshal(PublicKey(k))
	if got := string(b); got != `{"createdAt":"c","id":"key_1","lastRotatedAt":"r","name":"n","prefix":"lb_abc","scopes":[]}` {
		t.Errorf("plain: %s", got)
	}
	k.Scopes, k.ExpiresAt, k.RevokedAt = []string{"models:read"}, &when, &when
	b, _ = json.Marshal(PublicKey(k))
	if got := string(b); got != `{"createdAt":"c","expiresAt":"2026-01-02T00:00:00.000Z","id":"key_1","lastRotatedAt":"r","name":"n","prefix":"lb_abc","revokedAt":"2026-01-02T00:00:00.000Z","scopes":["models:read"]}` {
		t.Errorf("expired and revoked: %s", got)
	}
}

func TestValidScopes(t *testing.T) {
	if got, ok := ValidScopes([]string{"keys:read", "inference:chat", "keys:read"}); !ok || !slices.Equal(got, []string{"inference:chat", "keys:read"}) {
		t.Errorf("valid: %q %v", got, ok)
	}
	if got, ok := ValidScopes(nil); !ok || got == nil || len(got) != 0 {
		t.Errorf("none: %#v %v", got, ok)
	}
	for _, bad := range [][]string{{"keys:read", "keys:everything"}, {""}} {
		if got, ok := ValidScopes(bad); ok || got != nil {
			t.Errorf("%q: %q %v", bad, got, ok)
		}
	}
}
