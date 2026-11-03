package auth

import (
	"slices"
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
