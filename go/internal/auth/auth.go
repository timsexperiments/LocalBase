// Package auth resolves request principals and their permissions.
package auth

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Permissions in canonical order; scope lists are returned in this order.
var Permissions = []string{
	"inference:chat", "inference:embeddings", "inference:decision", "inference:image",
	"inference:video", "inference:speech", "inference:transcription", "models:read",
	"models:manage", "configuration:read", "configuration:manage", "keys:read",
	"keys:manage", "access:read", "access:manage", "sessions:read", "sessions:revoke",
	"system:read", "system:manage",
}

// DefaultKeyScopes are granted to new keys unless scopes are given.
var DefaultKeyScopes = Permissions[:8]

// Principal is who a request acts as. Kind: anonymous, api-key, environment, browser-session.
type Principal struct {
	Kind        string
	ID          string // api key id or browser owner id
	Name        string
	Permissions []string
}

func (p *Principal) Authenticated() bool { return p != nil && p.Kind != "anonymous" }

func (p *Principal) Can(permission string) bool {
	if p == nil {
		return false
	}
	for _, x := range p.Permissions {
		if x == permission {
			return true
		}
	}
	return false
}

// OwnerID matches the TS principalOwnerId (video and voice ownership).
func (p *Principal) OwnerID() string {
	switch p.Kind {
	case "api-key":
		return "api-key:" + p.ID
	case "environment":
		return "environment"
	}
	return p.ID
}

var Anonymous = &Principal{Kind: "anonymous"}

// Resolver turns request credentials into a principal.
type Resolver struct {
	Store  *store.Store
	EnvKey string // LOCALBASE_API_KEY; full access
	Mode   string // bearer | x-api-key | either
}

func bearer(r *http.Request) string {
	scheme, token, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// Token extracts the API key per the configured auth mode.
func (a *Resolver) Token(r *http.Request) string {
	xkey := strings.TrimSpace(r.Header.Get("X-Api-Key"))
	switch a.Mode {
	case "bearer":
		return bearer(r)
	case "x-api-key":
		return xkey
	}
	if t := bearer(r); t != "" {
		return t
	}
	return xkey
}

// IsBrowser: requests with Origin or Sec-Fetch-Site never authenticate with
// API keys, so a key pasted into a page cannot act as a browser identity.
func IsBrowser(r *http.Request) bool {
	return r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != ""
}

// ResolveAPI resolves API credentials (not browser sessions).
func (a *Resolver) ResolveAPI(ctx context.Context, r *http.Request) (*Principal, error) {
	token := a.Token(r)
	if token == "" || IsBrowser(r) {
		return Anonymous, nil
	}
	if a.EnvKey != "" && subtle.ConstantTimeCompare([]byte(token), []byte(a.EnvKey)) == 1 {
		return &Principal{Kind: "environment", Permissions: Permissions}, nil
	}
	return a.ResolveKey(ctx, token)
}

// ResolveKey resolves a raw API key, or Anonymous.
func (a *Resolver) ResolveKey(ctx context.Context, token string) (*Principal, error) {
	k, err := a.Store.ResolveAPIKey(ctx, token)
	if err != nil || k == nil {
		return Anonymous, err
	}
	return &Principal{Kind: "api-key", ID: k.ID, Name: k.Name, Permissions: Canonical(k.Scopes)}, nil
}

// Canonical filters to known permissions in canonical order, deduplicated.
func Canonical(scopes []string) []string {
	have := map[string]bool{}
	for _, s := range scopes {
		have[s] = true
	}
	out := []string{}
	for _, p := range Permissions {
		if have[p] {
			out = append(out, p)
		}
	}
	return out
}
