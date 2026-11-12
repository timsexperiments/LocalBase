package auth

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func oidcAt(id, issuer string) Registration {
	r := oidcReg(id)
	r.Issuer = issuer
	return r
}

func TestPreparePolicy(t *testing.T) {
	const a, b, c = "https://a.example.com", "https://b.example.com", "https://c.example.com"
	one := planDirect(nil, oidcAt("a", a))
	two := planDirect(nil, oidcAt("a", a), oidcAt("b", b))
	email := func(addr, issuer string) Binding {
		return Binding{Kind: "email", Role: "admin", Email: addr, Issuer: issuer}
	}
	policyWith := func(bindings ...Binding) *Policy {
		p := &Policy{Roles: []Role{{Name: "admin", Description: "Admin", Permissions: []string{"access:manage"}}}, Bindings: bindings}
		if err := p.Normalize(); err != nil {
			t.Fatal(err)
		}
		return p
	}
	code := func(err error) string {
		var ae *AccessError
		if errors.As(err, &ae) {
			return ae.Code
		}
		if err == nil {
			return ""
		}
		return err.Error()
	}
	for _, tc := range []struct {
		name     string
		p        *Policy
		cfg      *BrowserAccess
		existing *Policy
		caller   *Identity
		want     string // the AccessError code, or the error's text
		issuer   string // what the first binding names after
	}{
		{"no provider", policyWith(email("o@example.com", "")), nil, nil, nil, ErrProviderNotConfigured.Error(), ""},
		{"one provider names it", policyWith(email("o@example.com", "")), one, nil, nil, "", a},
		{"several, none named", policyWith(email("o@example.com", "")), two, nil, nil, "issuer-required", ""},
		{"several, the caller's own address", policyWith(email("o@example.com", "")), two, nil,
			&Identity{Issuer: b, Subject: "s", VerifiedEmail: "o@example.com"}, "", b},
		{"a new binding on an unknown provider", policyWith(email("o@example.com", c)), two, nil, nil, "issuer-unknown", c},
		{"the same binding, already stored", policyWith(email("o@example.com", c)), two, policyWith(email("o@example.com", c)), nil, "", c},
		{"Cloudflare names its team", policyWith(email("o@example.com", "")), cloudflare(nil), nil, nil, "", "https://team.cloudflareaccess.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := PreparePolicy(tc.p, tc.cfg, tc.existing, tc.caller)
			if got := code(err); got != tc.want {
				t.Fatalf("PreparePolicy: %q, want %q", got, tc.want)
			}
			if got := tc.p.Bindings[0].Issuer; tc.want == "" && got != tc.issuer {
				t.Errorf("the binding names %q, want %q", got, tc.issuer)
			}
		})
	}
}

func TestTestPolicy(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	const a = "https://a.example.com"
	cfg := planDirect([]string{"models:read"}, oidcAt("a", a))
	owner := Identity{Issuer: a, Subject: "s", VerifiedEmail: "owner@example.com"}

	if _, _, err := TestPolicy(ctx, st, nil, owner); !errors.Is(err, ErrProviderNotConfigured) {
		t.Errorf("no provider: %v", err)
	}
	d, configured, err := TestPolicy(ctx, st, cfg, owner)
	if err != nil || configured || !reflect.DeepEqual(d, &Decision{MatchedRoles: []string{}, Permissions: []string{"models:read"}}) {
		t.Errorf("no policy: %+v, %v, %v; want the provider-wide permissions", d, configured, err)
	}
	if err := ApplyPolicy(ctx, st, StarterPolicy("owner@example.com", a)); err != nil {
		t.Fatal(err)
	}
	d, configured, err = TestPolicy(ctx, st, cfg, owner)
	if err != nil || !configured || !reflect.DeepEqual(d.MatchedRoles, []string{"admin"}) || len(d.Permissions) != len(Permissions) {
		t.Errorf("the owner under the starter policy: %+v, %v, %v", d, configured, err)
	}
	d, configured, err = TestPolicy(ctx, st, cfg, Identity{Issuer: a, Subject: "x", VerifiedEmail: "else@example.com"})
	if err != nil || !configured || len(d.MatchedRoles) != 0 || len(d.Permissions) != 0 {
		t.Errorf("someone else under the starter policy: %+v, %v, %v", d, configured, err)
	}
}
