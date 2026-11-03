package auth

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Lockout rules for access management (a Go addition; TS keeps them in its
// browser UI only). Whoever makes a change, the API refuses one that would
// leave the caller without access:manage, disable or forget the caller's own
// record, or leave no identity able to manage access. `local-base access` on
// the host stays the recovery path and only warns.

// Guard is a snapshot of the policy and managed users the rules read.
type Guard struct {
	Policy        *Policy // nil: no policy (provider-wide permissions apply)
	Users         []GuardUser
	ProviderPerms []string // the provider-wide permissions without a policy
	// Issuers are the sign-in providers configured: nobody signs in through another.
	Issuers []string
	// KeyManagers are the active API keys with access:manage: principals
	// too, so they count when deciding whether anyone can still manage.
	KeyManagers []string
}

// GuardUser is a managed user with the identities linked to it.
type GuardUser struct {
	ID, Email, Status string
	Roles             []string
	Issuer            string      // the provider that can claim this record
	Identities        [][2]string // issuer, subject
}

// LoadGuard reads the current state. cfg is the browser access configuration, or nil.
func LoadGuard(ctx context.Context, st *store.Store, cfg *BrowserAccess) (*Guard, error) {
	policy, err := LoadPolicy(ctx, st.DB)
	if err != nil {
		return nil, err
	}
	g := &Guard{Policy: policy, Issuers: cfg.Issuers()}
	if cfg != nil {
		g.ProviderPerms = cfg.Permissions
	}
	keys, err := st.ListAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		if ActiveKey(k) && slices.Contains(k.Scopes, "access:manage") {
			g.KeyManagers = append(g.KeyManagers, k.ID)
		}
	}
	// Removed people too: a sign-in stays refused for them, whatever a binding says.
	users, err := listUsers(ctx, st, true)
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		gu := GuardUser{ID: u.ID, Email: strings.ToLower(u.Email), Status: u.Status, Roles: slices.Clone(u.Roles), Issuer: u.Issuer}
		rows, err := st.DB.QueryContext(ctx, `SELECT issuer, subject FROM auth_identities WHERE user_id = ?`, u.ID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var is, sub string
			if err := rows.Scan(&is, &sub); err != nil {
				rows.Close()
				return nil, err
			}
			gu.Identities = append(gu.Identities, [2]string{is, sub})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		g.Users = append(g.Users, gu)
	}
	return g, nil
}

// Clone copies the snapshot so a change can be tried on it.
func (g *Guard) Clone() *Guard {
	c := &Guard{ProviderPerms: g.ProviderPerms, Issuers: g.Issuers, KeyManagers: slices.Clone(g.KeyManagers)}
	if g.Policy != nil {
		p := *g.Policy
		p.Roles = slices.Clone(p.Roles)
		p.Bindings = slices.Clone(p.Bindings)
		c.Policy = &p
	}
	for _, u := range g.Users {
		u.Roles = slices.Clone(u.Roles)
		c.Users = append(c.Users, u)
	}
	return c
}

// User returns the managed user with an email (case-insensitive), or nil.
func (g *Guard) User(email string) *GuardUser {
	email = strings.ToLower(email)
	for i := range g.Users {
		if g.Users[i].Email == email {
			return &g.Users[i]
		}
	}
	return nil
}

func (g *Guard) rolePerms(name string) []string {
	for _, r := range g.Policy.Roles {
		if r.Name == name {
			return r.Permissions
		}
	}
	return nil
}

func (g *Guard) roleManages(name string) bool {
	return slices.Contains(g.rolePerms(name), "access:manage")
}

// refusedStatus is a person no sign-in may act as.
func refusedStatus(status string) bool { return status != "active" && status != "pending" }

// Managed counts the people who have not been removed.
func (g *Guard) Managed() int {
	n := 0
	for i := range g.Users {
		if g.Users[i].Status != "removed" {
			n++
		}
	}
	return n
}

// linked is the managed user an identity signs in as: the one it is linked to, else an
// invitation for its verified email from the provider it was made for (claimed on
// sign-in). A refused address counts whichever provider vouches for it: it stays out.
func (g *Guard) linked(id Identity) *GuardUser {
	for i := range g.Users {
		for _, l := range g.Users[i].Identities {
			if l == [2]string{id.Issuer, id.Subject} {
				return &g.Users[i]
			}
		}
	}
	if id.VerifiedEmail != "" {
		if u := g.User(id.VerifiedEmail); u != nil && (refusedStatus(u.Status) || u.Issuer == id.Issuer) {
			return u
		}
	}
	return nil
}

// CanManage reports whether an identity would have access:manage, resolved
// as ResolvePolicy resolves it.
func (g *Guard) CanManage(id Identity) bool {
	if !slices.Contains(g.Issuers, id.Issuer) {
		return false
	}
	if g.Policy == nil {
		return slices.Contains(g.ProviderPerms, "access:manage")
	}
	u := g.linked(id)
	if u != nil && refusedStatus(u.Status) {
		return false
	}
	if g.Policy.DefaultRole != nil && g.roleManages(*g.Policy.DefaultRole) {
		return true
	}
	email := strings.ToLower(id.VerifiedEmail)
	for _, b := range g.Policy.Bindings {
		if !g.roleManages(b.Role) {
			continue
		}
		switch b.Kind {
		case "subject":
			if b.Issuer == id.Issuer && b.Subject == id.Subject {
				return true
			}
		case "email":
			if email != "" && b.Email == email && b.Issuer == id.Issuer {
				return true
			}
		case "email-domain":
			if at := strings.LastIndex(email, "@"); at >= 0 && b.Domain == email[at+1:] && b.Issuer == id.Issuer {
				return true
			}
		}
	}
	if u != nil {
		for _, r := range u.Roles {
			if g.roleManages(r) {
				return true
			}
		}
	}
	return false
}

// HasManager reports whether some identity could still manage access.
func (g *Guard) HasManager() bool {
	if len(g.KeyManagers) > 0 {
		return true
	}
	if len(g.Issuers) == 0 {
		return false // nobody can sign in
	}
	if g.Policy == nil {
		return slices.Contains(g.ProviderPerms, "access:manage")
	}
	blocked := func(u *GuardUser) bool { return u != nil && refusedStatus(u.Status) }
	if g.Policy.DefaultRole != nil && g.roleManages(*g.Policy.DefaultRole) {
		return true
	}
	for _, b := range g.Policy.Bindings {
		if !g.roleManages(b.Role) || !slices.Contains(g.Issuers, b.Issuer) {
			continue
		}
		switch b.Kind {
		case "email-domain":
			return true
		case "email":
			if !blocked(g.User(b.Email)) {
				return true
			}
		case "subject":
			if !blocked(g.linked(Identity{Issuer: b.Issuer, Subject: b.Subject})) {
				return true
			}
		}
	}
	for i := range g.Users {
		u := &g.Users[i]
		if blocked(u) || !g.canSignIn(u) {
			continue
		}
		for _, r := range u.Roles {
			if g.roleManages(r) {
				return true
			}
		}
	}
	return false
}

// canSignIn reports whether a configured provider can reach this person.
func (g *Guard) canSignIn(u *GuardUser) bool {
	if slices.Contains(g.Issuers, u.Issuer) {
		return true
	}
	for _, l := range u.Identities {
		if slices.Contains(g.Issuers, l[0]) {
			return true
		}
	}
	return false
}

// Lockout errors: one stable code (409 would_lock_out_admins), a message each.
var (
	ErrSelfLockout = &AccessError{Code: "would-lock-out-admins", Message: "You can't remove your own access management: you would lose access:manage. Ask another administrator, or use `local-base access` on the host."}
	ErrSelfRecord  = &AccessError{Code: "would-lock-out-admins", Message: "You can't disable or forget your own account. Ask another administrator, or use `local-base access` on the host."}
	ErrSelfKey     = &AccessError{Code: "would-lock-out-admins", Message: "You can't remove access:manage from the API key you are using. Use another key, or `local-base keys` on the host."}
	ErrLastManager = &AccessError{Code: "would-lock-out-admins", Message: "This change would leave nobody able to manage access. Keep at least one active person, binding or API key with access:manage."}
)

// CheckChange applies change to a copy of g and refuses a lockout. caller is
// the signed-in identity making it, or nil for API keys and the host.
func (g *Guard) CheckChange(caller *Identity, change func(next *Guard)) error {
	next := g.Clone()
	change(next)
	if caller != nil && g.CanManage(*caller) && !next.CanManage(*caller) {
		return ErrSelfLockout
	}
	if g.HasManager() && !next.HasManager() {
		return ErrLastManager
	}
	return nil
}

// IsCaller reports whether a managed user is the caller's own record.
func (g *Guard) IsCaller(caller *Identity, email string) bool {
	if caller == nil {
		return false
	}
	u := g.User(email)
	if u == nil {
		return false
	}
	if l := g.linked(*caller); l != nil && l.ID == u.ID {
		return true
	}
	return caller.VerifiedEmail != "" && strings.EqualFold(caller.VerifiedEmail, email)
}

// ActiveKey reports a key that is neither revoked nor expired.
func ActiveKey(k store.APIKey) bool {
	if k.RevokedAt != nil {
		return false
	}
	if k.ExpiresAt == nil {
		return true
	}
	t, err := time.Parse(time.RFC3339Nano, *k.ExpiresAt)
	return err == nil && t.After(time.Now())
}

// CheckKeyChange refuses removing access:manage from the calling key (callerKey is its
// id, or ""), and a change that leaves nobody able to manage access. Rotating counts as
// removal unless the caller rotates its own key: the new secret goes to the caller, so
// whoever held the key loses it. scopes are the new permissions of a set-scopes.
func (g *Guard) CheckKeyChange(callerKey string, target *store.APIKey, action string, scopes []string) error {
	if !slices.Contains(target.Scopes, "access:manage") {
		return nil
	}
	self := callerKey == target.ID
	switch action {
	case "revoke":
	case "rotate":
		if self {
			return nil
		}
	case "set-scopes":
		if slices.Contains(scopes, "access:manage") {
			return nil
		}
		if self {
			return ErrSelfKey
		}
	default:
		return nil
	}
	return g.CheckChange(nil, func(next *Guard) {
		next.KeyManagers = slices.DeleteFunc(next.KeyManagers, func(id string) bool { return id == target.ID })
	})
}
