package auth

import (
	"slices"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// PublicKey is an API key's metadata as the command line and the gateway show it, never its hash.
func PublicKey(k *store.APIKey) map[string]any {
	scopes := k.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	m := map[string]any{"id": k.ID, "name": k.Name, "prefix": k.Prefix, "scopes": scopes, "createdAt": k.CreatedAt,
		"lastRotatedAt": k.LastRotatedAt}
	if k.ExpiresAt != nil {
		m["expiresAt"] = *k.ExpiresAt
	}
	if k.RevokedAt != nil {
		m["revokedAt"] = *k.RevokedAt
	}
	return m
}

// ValidScopes is scopes in canonical order (Canonical), or false when one isn't a permission.
func ValidScopes(scopes []string) ([]string, bool) {
	for _, sc := range scopes {
		if !slices.Contains(Permissions, sc) {
			return nil, false
		}
	}
	return Canonical(scopes), true
}
