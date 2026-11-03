package auth

import (
	"slices"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Nobody hands out more than they hold (DESIGN §13.6, item 5a). A caller may grant only
// permissions it has, may act only on a key whose permissions are all its own, and a key
// may not give anything a longer life than it has itself. `local-base keys` on the host
// is not bound by this: having the machine is the authority behind everything here.

// Missing is what want asks for that holds lacks, in canonical order.
func Missing(holds, want []string) []string {
	out := []string{}
	for _, p := range Canonical(want) {
		if !slices.Contains(holds, p) {
			out = append(out, p)
		}
	}
	return out
}

func grantRefused(missing []string) *AccessError {
	return &AccessError{"exceeds-caller-permissions", "You can only grant permissions you hold. You don't hold: " + joinComma(missing) + "."}
}

// ErrWiderKey refuses acting on a key with permissions the caller lacks. It does not
// name them: a caller that can't read keys learns nothing about this one.
var ErrWiderKey = &AccessError{"exceeds-caller-permissions", "This API key has permissions you don't hold, so you can't rotate, revoke or change it. Ask someone who holds them, or use `local-base keys` on the host."}

// KeyCaller is who is changing API keys.
type KeyCaller struct {
	Holds []string
	// Key is the API key making the change: nil for a signed-in person or the host,
	// whose authority does not end on a date.
	Key *store.APIKey
}

// KeyID is the calling key's id, or "".
func (c KeyCaller) KeyID() string {
	if c.Key == nil {
		return ""
	}
	return c.Key.ID
}

// keyExpiry is when a stored expiry falls. A value that can't be read counts as no
// expiry, which is the answer a limit refuses.
func keyExpiry(stored *string) (time.Time, bool) {
	if stored == nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, *stored)
	return t, err == nil
}

// checkLife refuses a life longer than the calling key's own. ok is false for a key
// that never expires.
func (c KeyCaller) checkLife(until time.Time, ok bool) error {
	if c.Key == nil {
		return nil
	}
	own, limited := keyExpiry(c.Key.ExpiresAt)
	if limited && (!ok || until.After(own)) {
		return &AccessError{"exceeds-caller-expiry", "This API key expires at " + *c.Key.ExpiresAt + ", and a key it creates or changes can't outlast it. Choose an earlier expiry, or use a signed-in session or `local-base keys` on the host."}
	}
	return nil
}

func (c KeyCaller) checkTarget(target *store.APIKey) error {
	if len(Missing(c.Holds, target.Scopes)) > 0 {
		return ErrWiderKey
	}
	return nil
}

// CheckCreate judges a new key: expiresDays is 0 for one that never expires.
func (c KeyCaller) CheckCreate(scopes []string, expiresDays int) error {
	if missing := Missing(c.Holds, scopes); len(missing) > 0 {
		return grantRefused(missing)
	}
	return c.checkLife(time.Now().Add(time.Duration(expiresDays)*24*time.Hour), expiresDays > 0)
}

// CheckRotate judges replacing a key's secret. The caller gets the new secret, so this
// is the same as being handed a key with those permissions and that expiry.
func (c KeyCaller) CheckRotate(target *store.APIKey) error {
	if err := c.checkTarget(target); err != nil {
		return err
	}
	until, ok := keyExpiry(target.ExpiresAt)
	return c.checkLife(until, ok)
}

// CheckRevoke judges revoking a key. It gives nothing, so only permissions count.
func (c KeyCaller) CheckRevoke(target *store.APIKey) error {
	return c.checkTarget(target)
}

// CheckSetScopes judges replacing a key's permissions. Adding to a key that outlasts
// the caller would give those permissions a longer life; removing from one would not.
func (c KeyCaller) CheckSetScopes(target *store.APIKey, scopes []string) error {
	if err := c.checkTarget(target); err != nil {
		return err
	}
	if missing := Missing(c.Holds, scopes); len(missing) > 0 {
		return grantRefused(missing)
	}
	if len(Missing(target.Scopes, scopes)) == 0 {
		return nil
	}
	until, ok := keyExpiry(target.ExpiresAt)
	return c.checkLife(until, ok)
}

// --- access management ----------------------------------------------------------------

func needs(holds, want []string, refusal func(missing []string) *AccessError) error {
	if missing := Missing(holds, want); len(missing) > 0 {
		return refusal(missing)
	}
	return nil
}

func refusedSignIn(missing []string) *AccessError {
	return &AccessError{"exceeds-caller-permissions", "Changing how people sign in can decide who holds any role, so it needs every permission a sign-in can grant. You don't hold: " + joinComma(missing) + ". Ask someone who holds them, or use `local-base access` on the host."}
}

func refusedPolicy(missing []string) *AccessError {
	return &AccessError{"exceeds-caller-permissions", "You can only change roles and bindings within the permissions you hold. This change also touches: " + joinComma(missing) + ". Ask someone who holds them, or use `local-base access` on the host."}
}

// ErrWiderUser refuses acting on a person with permissions the caller lacks. It does
// not name them, for the same reason as ErrWiderKey.
var ErrWiderUser = &AccessError{"exceeds-caller-permissions", "This person has permissions you don't hold, so you can't change them. Ask someone who holds them, or use `local-base access` on the host."}

func (g *Guard) role(name string) ([]string, bool) {
	if g.Policy == nil {
		return nil, false
	}
	for _, r := range g.Policy.Roles {
		if r.Name == name {
			return r.Permissions, true
		}
	}
	return nil, false
}

// signInReach is everything a sign-in could resolve to: every role, and the
// provider-wide permissions, which apply when there is no policy.
func (g *Guard) signInReach() []string {
	reach := slices.Clone(g.ProviderPerms)
	if g.Policy != nil {
		for _, r := range g.Policy.Roles {
			reach = append(reach, r.Permissions...)
		}
	}
	return reach
}

// userReach is what a managed person resolves to, as ResolvePolicy resolves it.
func (g *Guard) userReach(u *GuardUser) []string {
	if g.Policy == nil {
		return g.ProviderPerms
	}
	var reach []string
	add := func(name string) {
		perms, _ := g.role(name)
		reach = append(reach, perms...)
	}
	for _, name := range u.Roles {
		add(name)
	}
	if g.Policy.DefaultRole != nil {
		add(*g.Policy.DefaultRole)
	}
	domain := u.Email[strings.LastIndex(u.Email, "@")+1:]
	for _, b := range g.Policy.Bindings {
		switch b.Kind {
		case "email":
			if b.Email == u.Email {
				add(b.Role)
			}
		case "email-domain":
			if b.Domain == domain {
				add(b.Role)
			}
		case "subject":
			if slices.Contains(u.Identities, [2]string{b.Issuer, b.Subject}) {
				add(b.Role)
			}
		}
	}
	return reach
}

// CheckSignInChange judges a change to providers or email delivery. Those decide who
// proves to be whom, so whoever changes them could reach any role anyone holds: the
// caller must already hold all of it, and any provider-wide permissions it sets.
func (g *Guard) CheckSignInChange(holds, permissions []string) error {
	return needs(holds, append(g.signInReach(), permissions...), refusedSignIn)
}

// CheckGrantRoles judges giving someone these roles. An unknown role is left for the
// call that assigns it to refuse.
func (g *Guard) CheckGrantRoles(holds, roles []string) error {
	var want []string
	for _, name := range roles {
		perms, _ := g.role(name)
		want = append(want, perms...)
	}
	return needs(holds, want, grantRefused)
}

// CheckUser judges acting on a managed person. An unknown person is left for the call
// that acts on them to refuse.
func (g *Guard) CheckUser(holds []string, email string) error {
	if u := g.User(email); u != nil && len(Missing(holds, g.userReach(u))) > 0 {
		return ErrWiderUser
	}
	return nil
}

func rolesByName(p *Policy) map[string][]string {
	out := map[string][]string{}
	if p != nil {
		for _, r := range p.Roles {
			out[r.Name] = r.Permissions
		}
	}
	return out
}

func bindingSet(p *Policy) map[Binding]bool {
	out := map[Binding]bool{}
	if p != nil {
		for _, b := range p.Bindings {
			out[b] = true
		}
	}
	return out
}

func defaultRole(p *Policy) string {
	if p == nil || p.DefaultRole == nil {
		return ""
	}
	return *p.DefaultRole
}

// CheckPolicyChange judges replacing the policy with next (nil: clearing it). What
// stays the same needs nothing: a caller may leave a wider role alone. Everything that
// changes needs what it touches, before and after, so a caller can neither hand out a
// wider role nor take one away. Moving between no policy and a policy replaces the
// provider-wide permissions of everyone signing in.
func (g *Guard) CheckPolicyChange(holds []string, next *Policy) error {
	var want []string
	if (g.Policy == nil) != (next == nil) {
		want = append(want, g.ProviderPerms...)
	}
	before, after := rolesByName(g.Policy), rolesByName(next)
	for name, old := range before {
		if now, kept := after[name]; !kept || !slices.Equal(Canonical(old), Canonical(now)) {
			want = append(append(want, old...), now...)
		}
	}
	for name, now := range after {
		if _, existed := before[name]; !existed {
			want = append(want, now...)
		}
	}
	had, has := bindingSet(g.Policy), bindingSet(next)
	for b := range had {
		if !has[b] {
			want = append(want, before[b.Role]...)
		}
	}
	for b := range has {
		if !had[b] {
			want = append(want, after[b.Role]...)
		}
	}
	if old, now := defaultRole(g.Policy), defaultRole(next); old != now {
		want = append(append(want, before[old]...), after[now]...)
	}
	return needs(holds, want, refusedPolicy)
}
