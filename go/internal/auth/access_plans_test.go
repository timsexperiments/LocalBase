package auth

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
)

func oidcReg(id string) Registration {
	return Registration{Kind: "oidc", ID: id, Name: "Provider " + id, Issuer: "https://id.example.com", ClientID: "cid",
		ClientAuthentication: &ClientAuth{Kind: "none"}}
}

func planDirect(perms []string, regs ...Registration) *BrowserAccess {
	return &BrowserAccess{Provider: Provider{Kind: "direct", Registrations: regs}, Origin: testOrigin, Permissions: perms}
}

func cloudflare(perms []string) *BrowserAccess {
	return &BrowserAccess{Provider: Provider{Kind: "cloudflare-access", TeamDomain: "team.cloudflareaccess.com", Audience: "aud"},
		Origin: testOrigin, Permissions: perms}
}

// parseAccess reads a configuration the way LoadAccess does, without a file.
func parseAccess(t *testing.T, s string) *BrowserAccess {
	t.Helper()
	var a BrowserAccess
	if err := json.Unmarshal([]byte(s), &a); err != nil {
		t.Fatal(err)
	}
	return &a
}

// AccessChanged drives whether a change is audited. It compares what a save would keep, in the
// form the plans write: permissions canonical and registrations in id order. Everything else,
// case included, is compared exactly, since a save keeps it as given.
func TestAccessChangedComparesTheSavedForm(t *testing.T) {
	read, chat := "models:read", "inference:chat"
	github := func(space string) string {
		return `{"provider":{"kind":"direct","registrations":[{"kind":"github-oauth","id":"gh","name":"GitHub",` + space +
			`"clientId":"c","clientSecret":"s"}]},"origin":"https://lb.example.com","permissions":[]}`
	}
	for _, tc := range []struct {
		name          string
		current, next *BrowserAccess
		want          bool
	}{
		{"both off", nil, nil, false},
		{"turned on", nil, planDirect(nil, oidcReg("a")), true},
		{"turned off", planDirect(nil, oidcReg("a")), nil, true},
		{"the same", planDirect([]string{read}, oidcReg("a")), planDirect([]string{read}, oidcReg("a")), false},
		{"permissions in another order", planDirect([]string{read, chat}, oidcReg("a")), planDirect([]string{chat, read}, oidcReg("a")), false},
		{"a permission repeated", planDirect([]string{read}, oidcReg("a")), planDirect([]string{read, read}, oidcReg("a")), false},
		{"nil and empty permissions", planDirect(nil, oidcReg("a")), planDirect([]string{}, oidcReg("a")), false},
		{"a permission added", planDirect([]string{read}, oidcReg("a")), planDirect([]string{read, chat}, oidcReg("a")), true},
		{"registrations in another order", planDirect(nil, oidcReg("a"), oidcReg("b")), planDirect(nil, oidcReg("b"), oidcReg("a")), false},
		{"a registration added", planDirect(nil, oidcReg("a")), planDirect(nil, oidcReg("a"), oidcReg("b")), true},
		{"the origin differs in case", planDirect(nil, oidcReg("a")), &BrowserAccess{Provider: Provider{Kind: "direct",
			Registrations: []Registration{oidcReg("a")}}, Origin: "https://LB.example.com"}, true},
		{"a name differs in case", planDirect(nil, oidcReg("a")), planDirect(nil, func() Registration {
			r := oidcReg("a")
			r.Name = "PROVIDER a"
			return r
		}()), true},
		{"a client secret differs", planDirect(nil, func() Registration {
			r := oidcReg("a")
			r.ClientAuthentication = &ClientAuth{Kind: "client-secret-basic", ClientSecret: "one"}
			return r
		}()), planDirect(nil, func() Registration {
			r := oidcReg("a")
			r.ClientAuthentication = &ClientAuth{Kind: "client-secret-basic", ClientSecret: "two"}
			return r
		}()), true},
		{"Cloudflare for direct", cloudflare(nil), planDirect(nil, oidcReg("a")), true},
		{"a GitHub registration spaced differently", parseAccess(t, github("")), parseAccess(t, github("\n  ")), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AccessChanged(tc.current, tc.next); got != tc.want {
				t.Errorf("AccessChanged = %v, want %v", got, tc.want)
			}
			if got := AccessChanged(tc.next, tc.current); got != tc.want {
				t.Errorf("AccessChanged the other way = %v, want %v", got, tc.want)
			}
		})
	}
}

// Comparing leaves both configurations as they were.
func TestAccessChangedChangesNothing(t *testing.T) {
	a := planDirect([]string{"models:read", "inference:chat", "models:read"}, oidcReg("b"), oidcReg("a"))
	b := planDirect([]string{"inference:chat"}, oidcReg("a"))
	before, _ := json.Marshal(a)
	AccessChanged(a, b)
	if after, _ := json.Marshal(a); string(after) != string(before) {
		t.Errorf("the configuration changed:\nwas %s\nnow %s", before, after)
	}
}

func TestPlanCloudflare(t *testing.T) {
	t.Run("plans the provider with canonical permissions", func(t *testing.T) {
		next, changed, err := PlanCloudflare(planDirect(nil, oidcReg("a")), "team.cloudflareaccess.com", "aud", testOrigin,
			[]string{"models:read", "inference:chat", "models:read"})
		if err != nil || !changed {
			t.Fatalf("got changed %v, err %v", changed, err)
		}
		want := cloudflare(Canonical([]string{"inference:chat", "models:read"}))
		if !reflect.DeepEqual(next, want) {
			t.Errorf("next = %+v, want %+v", next, want)
		}
	})
	t.Run("the same again is no change", func(t *testing.T) {
		_, changed, err := PlanCloudflare(cloudflare([]string{"models:read"}), "team.cloudflareaccess.com", "aud", testOrigin,
			[]string{"models:read"})
		if err != nil || changed {
			t.Errorf("got changed %v, err %v; want no change", changed, err)
		}
	})
	for _, tc := range []struct {
		name, team, origin string
		perms              []string
		want               string
	}{
		{"a bad team domain", "Bad", testOrigin, nil, "provider: invalid Cloudflare Access settings"},
		{"a bad origin", "team.cloudflareaccess.com", "http://x", nil,
			"origin: Expected an exact HTTPS origin without path, credentials, or trailing slash."},
		// Checked as given, before the permissions are made canonical (which would drop it).
		{"an unknown permission", "team.cloudflareaccess.com", testOrigin, []string{"bogus"}, "permissions: unknown permission bogus"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next, changed, err := PlanCloudflare(nil, tc.team, "aud", tc.origin, tc.perms)
			var invalid *ValidationError
			if !errors.As(err, &invalid) || invalid.Message != tc.want || next != nil || changed {
				t.Errorf("got %v, %v, %v; want a ValidationError %q", next, changed, err, tc.want)
			}
		})
	}
}

func TestPlanRegistration(t *testing.T) {
	t.Run("replaces Cloudflare", func(t *testing.T) {
		next, changed, err := PlanRegistration(cloudflare(nil), oidcReg("a"), testOrigin, nil)
		if err != nil || !changed || !reflect.DeepEqual(next, planDirect([]string{}, oidcReg("a"))) {
			t.Errorf("got %+v, %v, %v", next, changed, err)
		}
	})
	t.Run("replaces the one with its id and keeps id order", func(t *testing.T) {
		current := planDirect(nil, oidcReg("c"), oidcReg("a"))
		replacement := oidcReg("a")
		replacement.Name = "Renamed"
		next, changed, err := PlanRegistration(current, replacement, testOrigin, nil)
		if err != nil || !changed || !reflect.DeepEqual(next, planDirect([]string{}, replacement, oidcReg("c"))) {
			t.Errorf("got %+v, %v, %v", next, changed, err)
		}
		if !reflect.DeepEqual(current, planDirect(nil, oidcReg("c"), oidcReg("a"))) {
			t.Errorf("the current configuration changed: %+v", current)
		}
	})
	t.Run("the same again is no change", func(t *testing.T) {
		_, changed, err := PlanRegistration(planDirect([]string{"models:read"}, oidcReg("a")), oidcReg("a"), testOrigin, []string{"models:read"})
		if err != nil || changed {
			t.Errorf("got changed %v, err %v; want no change", changed, err)
		}
	})
	t.Run("a second magic link is refused", func(t *testing.T) {
		current := planDirect(nil, Registration{Kind: "magic-link", ID: "email", Name: "Email"})
		_, _, err := PlanRegistration(current, Registration{Kind: "magic-link", ID: "email2", Name: "Email"}, testOrigin, nil)
		var invalid *ValidationError
		if !errors.As(err, &invalid) || invalid.Message != "Only one magic-link registration may be configured." {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("an invalid registration is refused", func(t *testing.T) {
		bad := oidcReg("a")
		bad.Issuer = "http://id.example.com"
		next, changed, err := PlanRegistration(nil, bad, testOrigin, nil)
		var invalid *ValidationError
		if !errors.As(err, &invalid) || invalid.Message != "Expected an HTTPS OIDC issuer without credentials, query, or fragment." ||
			next != nil || changed {
			t.Errorf("got %v, %v, %v", next, changed, err)
		}
	})
}

func TestCheckMagicLink(t *testing.T) {
	email := &EmailConfig{}
	magic := func(id string) Registration { return Registration{Kind: "magic-link", ID: id, Name: "Email"} }
	for _, tc := range []struct {
		name    string
		current *BrowserAccess
		email   *EmailConfig
		want    error
	}{
		{"no email delivery", nil, nil, ErrEmailNotConfigured},
		// Email delivery is checked first, as the command line always has.
		{"no email delivery, and another magic link", planDirect(nil, magic("other")), nil, ErrEmailNotConfigured},
		{"another magic link", planDirect(nil, oidcReg("a"), magic("other")), email, &MagicLinkExistsError{"other"}},
		{"the same magic link again", planDirect(nil, magic("email")), email, nil},
		{"none yet", planDirect(nil, oidcReg("a")), email, nil},
		{"Cloudflare", cloudflare(nil), email, nil},
		{"browser access off", nil, email, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := CheckMagicLink(tc.current, tc.email, "email"); !reflect.DeepEqual(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestPlanRemoveRegistration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		current *BrowserAccess
	}{
		{"browser access off", nil},
		{"Cloudflare", cloudflare(nil)},
		{"another id", planDirect(nil, oidcReg("a"))},
	} {
		t.Run("not found: "+tc.name, func(t *testing.T) {
			next, disable, err := PlanRemoveRegistration(tc.current, "b")
			if !errors.Is(err, ErrRegistrationNotFound) || next != nil || disable {
				t.Errorf("got %v, %v, %v", next, disable, err)
			}
		})
	}
	t.Run("the last one disables", func(t *testing.T) {
		next, disable, err := PlanRemoveRegistration(planDirect(nil, oidcReg("a")), "a")
		if err != nil || !disable || next != nil {
			t.Errorf("got %v, %v, %v", next, disable, err)
		}
	})
	t.Run("one of several", func(t *testing.T) {
		current := planDirect([]string{"models:read"}, oidcReg("a"), oidcReg("b"))
		next, disable, err := PlanRemoveRegistration(current, "a")
		if err != nil || disable || !reflect.DeepEqual(next, planDirect([]string{"models:read"}, oidcReg("b"))) {
			t.Errorf("got %+v, %v, %v", next, disable, err)
		}
		if ids := []string{current.Provider.Registrations[0].ID, current.Provider.Registrations[1].ID}; !slices.Equal(ids, []string{"a", "b"}) {
			t.Errorf("the current configuration changed: %v", ids)
		}
	})
}
