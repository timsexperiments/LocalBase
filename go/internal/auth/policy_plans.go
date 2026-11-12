package auth

import (
	"context"
	"errors"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// The policy rules both callers apply (see access_plans.go for the shape). Applying, clearing,
// adopting an issuer and the starter policy already live in this package (ApplyPolicy, ClearPolicy,
// AdoptBindingIssuer, StarterPolicy); what was duplicated was the check before an apply and the
// policy test.

// ErrProviderNotConfigured: a policy is about who signs in, so applying or testing one needs a
// sign-in provider.
var ErrProviderNotConfigured = errors.New("no browser identity provider is configured")

// PreparePolicy is what both callers do to a policy between parsing it and applying it. p must
// already be normalized: each caller runs Normalize first, at parse, since what it refuses is said
// before anything else (and the gateway answers ErrPolicyLockout its own way). cfg is the browser
// access configuration, existing the stored policy (nil for none), and caller the person applying
// it, or nil from the command line.
//
// It names the provider on each email and domain binding that has none (BindIssuers: the caller's
// own, or the only one), then refuses a new binding on a provider that isn't configured
// (CheckBindingIssuers: one the stored policy already has may stay). Both refusals are
// AccessErrors. The caller's own checks (the gateway's revision, authority and lockout) run on the
// prepared policy afterwards.
func PreparePolicy(p *Policy, cfg *BrowserAccess, existing *Policy, caller *Identity) error {
	if cfg == nil {
		return ErrProviderNotConfigured
	}
	issuers := cfg.Issuers()
	if err := p.BindIssuers(issuers, caller); err != nil {
		return err
	}
	if len(issuers) == 0 { // with no provider to name, there is nothing to check a name against
		return nil
	}
	return (&Guard{Policy: existing, Issuers: issuers}).CheckBindingIssuers(p)
}

// TestPolicy is what the policy would give id: the roles it matches and the permissions they
// grant, or, with no policy, the provider-wide permissions. policyConfigured says whether a policy
// is stored. It changes nothing (ResolvePolicy without claiming).
func TestPolicy(ctx context.Context, st *store.Store, cfg *BrowserAccess, id Identity) (d *Decision, policyConfigured bool, err error) {
	if cfg == nil {
		return nil, false, ErrProviderNotConfigured
	}
	if d, err = ResolvePolicy(ctx, st, id, false); err != nil {
		return nil, false, err
	}
	if d == nil {
		d = &Decision{MatchedRoles: []string{}, Permissions: cfg.Permissions}
	}
	policy, err := LoadPolicy(ctx, st.DB)
	if err != nil {
		return nil, false, err
	}
	return d, policy != nil, nil
}
