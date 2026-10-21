package auth

import (
	"context"
	"slices"
	"strings"

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
}

// GuardUser is a managed user with the identities linked to it.
type GuardUser struct {
	ID, Email, Status string
	Roles             []string
	Identities        [][2]string // issuer, subject
}

// LoadGuard reads the current state.
func LoadGuard(ctx context.Context, st *store.Store, providerPerms []string) (*Guard, error) {
	policy, err := LoadPolicy(ctx, st.DB)
	if err != nil {
		return nil, err
	}
	g := &Guard{Policy: policy, ProviderPerms: providerPerms}
	users, err := ListUsers(ctx, st)
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		gu := GuardUser{ID: u.ID, Email: strings.ToLower(u.Email), Status: u.Status, Roles: slices.Clone(u.Roles)}
		rows, err := st.DB.QueryContext(ctx, `SELECT issuer, subject FROM auth_identities WHERE user_id = ?`, u.ID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var is, sub string
			rows.Scan(&is, &sub)
			gu.Identities = append(gu.Identities, [2]string{is, sub})
		}
		rows.Close()
		g.Users = append(g.Users, gu)
	}
	return g, nil
}

// Clone copies the snapshot so a change can be tried on it.
func (g *Guard) Clone() *Guard {
	c := &Guard{ProviderPerms: g.ProviderPerms}
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

func (g *Guard) roleManages(name string) bool { return slices.Contains(g.rolePerms(name), "access:manage") }

// linked is the managed user an identity signs in as: the one it is linked
// to, else an invitation for its verified email (claimed on sign-in).
func (g *Guard) linked(id Identity) *GuardUser {
	for i := range g.Users {
		for _, l := range g.Users[i].Identities {
			if l == [2]string{id.Issuer, id.Subject} {
				return &g.Users[i]
			}
		}
	}
	if id.VerifiedEmail != "" {
		return g.User(id.VerifiedEmail)
	}
	return nil
}

// CanManage reports whether an identity would have access:manage, resolved
// as ResolvePolicy resolves it.
func (g *Guard) CanManage(id Identity) bool {
	if g.Policy == nil {
		return slices.Contains(g.ProviderPerms, "access:manage")
	}
	u := g.linked(id)
	if u != nil && u.Status != "active" && u.Status != "pending" {
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
			if email != "" && b.Email == email {
				return true
			}
		case "email-domain":
			if at := strings.LastIndex(email, "@"); at >= 0 && b.Domain == email[at+1:] {
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
	if g.Policy == nil {
		return slices.Contains(g.ProviderPerms, "access:manage")
	}
	blocked := func(u *GuardUser) bool { return u != nil && u.Status != "active" && u.Status != "pending" }
	if g.Policy.DefaultRole != nil && g.roleManages(*g.Policy.DefaultRole) {
		return true
	}
	for _, b := range g.Policy.Bindings {
		if !g.roleManages(b.Role) {
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
		if blocked(u) {
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

// Lockout errors.
var (
	ErrSelfLockout = &AccessError{Code: "self-lockout", Message: "You can't remove your own access management: you would lose access:manage. Ask another administrator, or use `local-base access` on the host."}
	ErrSelfRecord  = &AccessError{Code: "self-lockout", Message: "You can't disable or forget your own account. Ask another administrator, or use `local-base access` on the host."}
	ErrLastManager = &AccessError{Code: "last-manager", Message: "This change would leave nobody able to manage access. Keep at least one active person or binding with access:manage."}
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
