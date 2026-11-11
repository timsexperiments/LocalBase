package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

// The plan cores for browser access: each takes the current configuration (nil when browser
// access is off) and the change, and returns the configuration it would leave, validated. None of
// them reads or writes a file, so a caller runs its own checks on the result (the gateway's
// lockout) before it writes it with SaveAccess, or removes the file with DisableAccess. Permissions
// arrive decided: a caller that defaults them does so before the plan.

// ValidationError is a configuration a plan refused: Message says why, in the words Validate uses.
type ValidationError struct{ Message string }

func (e *ValidationError) Error() string { return e.Message }

// ErrRegistrationNotFound: no direct registration has the id.
var ErrRegistrationNotFound = errors.New("registration not found")

// ErrEmailNotConfigured: magic-link sign-in needs email delivery configured first.
var ErrEmailNotConfigured = errors.New("email delivery is not configured")

// MagicLinkExistsError: another magic-link registration, ID, is configured. Only one may be.
type MagicLinkExistsError struct{ ID string }

func (e *MagicLinkExistsError) Error() string {
	return "magic-link registration " + e.ID + " is configured"
}

// AccessChanged reports whether next differs from current in what a save would keep; either may be
// nil, for browser access off. Both are compared in the form the plans write: permissions
// canonical (so an order or a repeat isn't a change) and registrations in id order (so a file
// written in another order isn't). Everything else is compared exactly, case included, since a
// save keeps it as given: an origin or a name that differs only in case is a change.
func AccessChanged(current, next *BrowserAccess) bool {
	if current == nil || next == nil {
		return current != next
	}
	return !bytes.Equal(savedForm(current), savedForm(next))
}

func savedForm(a *BrowserAccess) []byte {
	c := *a
	c.Permissions = Canonical(a.Permissions)
	c.Provider.Registrations = slices.Clone(a.Provider.Registrations)
	slices.SortStableFunc(c.Provider.Registrations, func(x, y Registration) int { return strings.Compare(x.ID, y.ID) })
	b, _ := json.Marshal(&c)
	return b
}

// PlanCloudflare plans Cloudflare Access sign-in, replacing whatever is configured. The settings
// are checked as given; the permissions come back canonical.
func PlanCloudflare(current *BrowserAccess, teamDomain, audience, origin string, permissions []string) (next *BrowserAccess, changed bool, err error) {
	next = &BrowserAccess{Provider: Provider{Kind: "cloudflare-access", TeamDomain: teamDomain, Audience: audience},
		Origin: origin, Permissions: permissions}
	if err := next.Validate(); err != nil {
		return nil, false, &ValidationError{err.Error()}
	}
	next.Permissions = Canonical(permissions)
	return next, AccessChanged(current, next), nil
}

// PlanRegistration plans adding reg as a direct registration, or replacing the one with its id. A
// Cloudflare provider is replaced. An OpenID Connect, GitHub or magic-link registration all plan
// this way; a magic-link one is checked with CheckMagicLink first.
func PlanRegistration(current *BrowserAccess, reg Registration, origin string, permissions []string) (next *BrowserAccess, changed bool, err error) {
	next = UpsertRegistration(current, reg, origin, permissions)
	if err := next.Validate(); err != nil {
		return nil, false, &ValidationError{err.Error()}
	}
	return next, AccessChanged(current, next), nil
}

// CheckMagicLink checks that a magic-link registration with the id may be added: email delivery
// must be configured (email is nil when it isn't) and no other magic-link registration may be.
// It plans nothing; PlanRegistration does.
func CheckMagicLink(current *BrowserAccess, email *EmailConfig, id string) error {
	if email == nil {
		return ErrEmailNotConfigured
	}
	if current != nil && current.Provider.Kind == "direct" {
		for _, r := range current.Provider.Registrations {
			if r.Kind == "magic-link" && r.ID != id {
				return &MagicLinkExistsError{r.ID}
			}
		}
	}
	return nil
}

// PlanRemoveRegistration plans removing the direct registration with the id. When it was the last
// one, disable is true and next is nil: the caller turns browser access off with DisableAccess
// instead of saving. disable doesn't mean "changed"; a removal always changes the configuration.
func PlanRemoveRegistration(current *BrowserAccess, id string) (next *BrowserAccess, disable bool, err error) {
	state, next := RemoveRegistration(current, id)
	switch state {
	case "not-found":
		return nil, false, ErrRegistrationNotFound
	case "disabled":
		return nil, true, nil
	}
	return next, false, nil
}
