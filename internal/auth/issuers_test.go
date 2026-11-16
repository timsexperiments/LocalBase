package auth

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

const (
	issuerA = "https://a.example.com"
	issuerB = "https://b.example.com"
)

func direct(t *testing.T, issuers ...string) *BrowserAccess {
	t.Helper()
	cfg := &BrowserAccess{Origin: testOrigin, Permissions: []string{}, Provider: Provider{Kind: "direct"}}
	for i, issuer := range issuers {
		cfg.Provider.Registrations = append(cfg.Provider.Registrations, Registration{Kind: "oidc", ID: "p" + string(rune('a'+i)), Name: "P", Issuer: issuer,
			ClientID: "cid", ClientAuthentication: &ClientAuth{Kind: "none"}})
	}
	return cfg
}

func decision(t *testing.T, st *store.Store, id Identity, claim bool) []string {
	t.Helper()
	d, err := ResolvePolicy(context.Background(), st, id, claim)
	if err != nil {
		t.Fatal(err)
	}
	if d == nil {
		return nil
	}
	return d.Permissions
}

func TestIssuersOfAConfiguration(t *testing.T) {
	var none *BrowserAccess
	if got := none.Issuers(); got != nil {
		t.Errorf("no configuration: %v", got)
	}
	cf := &BrowserAccess{Provider: Provider{Kind: "cloudflare-access", TeamDomain: "team.cloudflareaccess.com", Audience: "aud"}, Origin: testOrigin}
	if got := cf.Issuers(); !slices.Equal(got, []string{"https://team.cloudflareaccess.com"}) {
		t.Errorf("cloudflare: %v", got)
	}
	mixed := direct(t, issuerB, issuerA, issuerA)
	mixed.Provider.Registrations = append(mixed.Provider.Registrations, Registration{Kind: "magic-link", ID: "email", Name: "Email"},
		Registration{Kind: "github-oauth", ID: "gh", Name: "GitHub"})
	if got := mixed.Issuers(); !slices.Equal(got, []string{issuerA, issuerB, MagicIssuer(testOrigin)}) {
		t.Errorf("direct: %v", got)
	}
}

// A provider never used before can't sign in as someone by claiming their address: the
// real sign-in flow, end to end, from the provider's token to the session's permissions.
func TestNewProviderCannotClaimABoundAddress(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	f := newFakeIdP(t)
	f.claims = map[string]any{"email": "owner@example.com", "sub": "someone-else"}
	// The owner's binding trusts their own provider. This one claims the same address.
	bind := func(issuer string) {
		t.Helper()
		if err := ApplyPolicy(ctx, st, &Policy{Roles: []Role{{Name: "owner", Permissions: Permissions}},
			Bindings: []Binding{{Kind: "email", Role: "owner", Email: "owner@example.com", Issuer: issuer}}}); err != nil {
			t.Fatal(err)
		}
	}
	bind("https://accounts.example.com")
	reg := Registration{Kind: "oidc", ID: "other", Name: "Other", Issuer: f.srv.URL, ClientID: "cid",
		ClientAuthentication: &ClientAuth{Kind: "client-secret-basic", ClientSecret: "shh"}}
	b := browserFor(t, st, reg)
	b.Client = f.srv.Client()
	if resp := login(t, b, f, nil); cookieNamed(resp, SessionCookie) != nil {
		t.Fatalf("a provider that claims a bound address got a session: %d", resp.StatusCode)
	}
	if d := b.Authorize(ctx, &Identity{Issuer: f.srv.URL, Subject: "someone-else", VerifiedEmail: "owner@example.com"}); len(d.Permissions) != 0 || len(d.MatchedRoles) != 0 {
		t.Fatalf("a provider that claims a bound address got %v %v", d.MatchedRoles, d.Permissions)
	}
	// The same sign-in, when that provider is the one the binding trusts, is the owner.
	bind(f.srv.URL)
	session := cookieNamed(login(t, b, f, nil), SessionCookie)
	if session == nil {
		t.Fatal("no session for the trusted provider")
	}
	id, _ := b.Authenticate(get("/app/session", session))
	if d := b.Authorize(ctx, id); len(d.Permissions) != len(Permissions) {
		t.Errorf("the trusted provider got %v", d.Permissions)
	}
}

func TestBindingsMatchOnlyTheirIssuer(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := ApplyPolicy(ctx, st, &Policy{Roles: []Role{{Name: "a", Permissions: []string{"access:manage"}}, {Name: "b", Permissions: []string{"inference:chat"}}, {Name: "c", Permissions: []string{"models:read"}}},
		Bindings: []Binding{
			{Kind: "email", Role: "a", Email: "x@example.com", Issuer: issuerA},
			{Kind: "email-domain", Role: "b", Domain: "example.com", Issuer: issuerA},
			{Kind: "email", Role: "c", Email: "both@example.org", Issuer: issuerA},
			{Kind: "email", Role: "c", Email: "both@example.org", Issuer: issuerB},
			{Kind: "email", Role: "c", Email: "nobody@example.org"},
		}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		id    Identity
		wants string
	}{
		{"email and domain, the right provider", Identity{Issuer: issuerA, Subject: "s", VerifiedEmail: "x@example.com"}, "inference:chat,access:manage"},
		{"email and domain, another provider", Identity{Issuer: issuerB, Subject: "s", VerifiedEmail: "x@example.com"}, ""},
		{"domain only, the right provider", Identity{Issuer: issuerA, Subject: "t", VerifiedEmail: "y@example.com"}, "inference:chat"},
		{"domain only, another provider", Identity{Issuer: issuerB, Subject: "t", VerifiedEmail: "y@example.com"}, ""},
		{"two providers trusted, the first", Identity{Issuer: issuerA, Subject: "u", VerifiedEmail: "both@example.org"}, "models:read"},
		{"two providers trusted, the second", Identity{Issuer: issuerB, Subject: "u", VerifiedEmail: "both@example.org"}, "models:read"},
		{"no provider named", Identity{Issuer: issuerA, Subject: "v", VerifiedEmail: "nobody@example.org"}, ""},
	} {
		if got := strings.Join(decision(t, st, tc.id, false), ","); got != tc.wants {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.wants)
		}
	}
	// What was stored is what comes back, with the provider, each on its own binding.
	p, err := LoadPolicy(ctx, st.DB)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Bindings) != 5 {
		t.Fatalf("bindings: %+v", p.Bindings)
	}
	// It survives being applied again unchanged, and clearing leaves nothing behind.
	rev := *Revision(p)
	if err := ApplyPolicy(ctx, st, p); err != nil {
		t.Fatal(err)
	}
	again, _ := LoadPolicy(ctx, st.DB)
	if *Revision(again) != rev {
		t.Errorf("revision changed: %+v", again.Bindings)
	}
	if _, err := ClearPolicy(ctx, st); err != nil {
		t.Fatal(err)
	}
	var left int
	st.DB.QueryRow(`SELECT COUNT(*) FROM go_binding_issuers`).Scan(&left)
	if left != 0 {
		t.Errorf("%d names left after clearing the policy", left)
	}
}

func TestBindingIssuerMustBeWellFormed(t *testing.T) {
	for _, issuer := range []string{"http://a.example.com", "not a url", "https://a.example.com?x=1", "https://a.example.com#x", "https://" + strings.Repeat("a", 2100)} {
		p := &Policy{Roles: []Role{{Name: "a", Permissions: []string{"access:manage"}}}, Bindings: []Binding{{Kind: "email", Role: "a", Email: "x@example.com", Issuer: issuer}}}
		if p.Normalize() == nil {
			t.Errorf("email binding with issuer %.40q passed", issuer)
		}
		d := &Policy{Roles: []Role{{Name: "a", Permissions: []string{"access:manage"}}}, Bindings: []Binding{{Kind: "email-domain", Role: "a", Domain: "example.com", Issuer: issuer}}}
		if d.Normalize() == nil {
			t.Errorf("domain binding with issuer %.40q passed", issuer)
		}
	}
}

// Only the provider a person was invited through can claim their record.
func TestInvitationIsClaimedOnlyByItsIssuer(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := ApplyPolicy(ctx, st, &Policy{Roles: []Role{{Name: "boss", Permissions: []string{"access:manage"}}, {Name: "chat", Permissions: []string{"inference:chat"}}},
		Bindings: []Binding{{Kind: "subject", Role: "boss", Issuer: issuerA, Subject: "boss"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := InviteUser(ctx, st, "friend@example.com", []string{"chat"}, issuerA); err != nil {
		t.Fatal(err)
	}
	wrong := Identity{Issuer: issuerB, Subject: "impostor", VerifiedEmail: "friend@example.com"}
	if got := decision(t, st, wrong, true); len(got) != 0 {
		t.Fatalf("another provider claimed the invitation: %v", got)
	}
	var links int
	st.DB.QueryRow(`SELECT COUNT(*) FROM auth_identities`).Scan(&links)
	users, _ := ListUsers(ctx, st)
	if links != 0 || users[0].Status != "pending" {
		t.Fatalf("a refused claim changed the record: %d links, %s", links, users[0].Status)
	}
	right := Identity{Issuer: issuerA, Subject: "friend", VerifiedEmail: "friend@example.com"}
	if got := decision(t, st, right, true); strings.Join(got, ",") != "inference:chat" {
		t.Fatalf("the invited provider: %v", got)
	}
	users, _ = ListUsers(ctx, st)
	if users[0].Status != "active" || users[0].Issuer != issuerA {
		t.Errorf("after the claim: %+v", users[0])
	}
	// Once claimed, the other provider still can't join the record.
	if got := decision(t, st, wrong, true); len(got) != 0 {
		t.Errorf("another provider joined an active record: %v", got)
	}
	// An invitation naming no provider can't be claimed until it names one.
	if _, err := InviteUser(ctx, st, "unnamed@example.com", []string{"chat"}, ""); err != nil {
		t.Fatal(err)
	}
	if got := decision(t, st, Identity{Issuer: issuerA, Subject: "u", VerifiedEmail: "unnamed@example.com"}, true); len(got) != 0 {
		t.Errorf("an invitation with no provider was claimed: %v", got)
	}
	// Removing a person takes their provider with them, and a new invitation starts afresh.
	if _, err := InviteUser(ctx, st, "again@example.com", []string{"chat"}, issuerA); err != nil {
		t.Fatal(err)
	}
	names := func() int {
		var n int
		st.DB.QueryRow(`SELECT COUNT(*) FROM go_invitation_issuers i JOIN auth_user_emails e ON e.user_id = i.user_id WHERE e.email = 'again@example.com'`).Scan(&n)
		return n
	}
	if names() != 1 {
		t.Fatal("the invitation's provider wasn't recorded")
	}
	if _, err := RemoveUser(ctx, st, "again@example.com"); err != nil {
		t.Fatal(err)
	}
	if names() != 0 {
		t.Errorf("removing a person left their provider")
	}
	if _, err := InviteUser(ctx, st, "again@example.com", []string{"chat"}, issuerB); err != nil {
		t.Fatal(err)
	}
	if got := decision(t, st, Identity{Issuer: issuerB, Subject: "a2", VerifiedEmail: "again@example.com"}, true); len(got) != 1 {
		t.Errorf("a new invitation for a removed person: %v", got)
	}
	if got := decision(t, st, Identity{Issuer: issuerA, Subject: "a3", VerifiedEmail: "again@example.com"}, true); len(got) != 0 {
		t.Errorf("the provider of the old invitation: %v", got)
	}
	// A rollback can remove someone without touching the provider's name. Inviting them
	// again still replaces it.
	if _, err := InviteUser(ctx, st, "rolled@example.com", []string{"chat"}, issuerA); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`UPDATE auth_users SET status = 'removed' WHERE id = (SELECT user_id FROM auth_user_emails WHERE email = 'rolled@example.com')`); err != nil {
		t.Fatal(err)
	}
	if u, err := InviteUser(ctx, st, "rolled@example.com", []string{"chat"}, issuerB); err != nil || u.Issuer != issuerB {
		t.Errorf("inviting again after a rollback removed them: %+v %v", u, err)
	}
}

// Someone removed isn't waiting for a provider, and isn't given one.
func TestRemovedPeopleAreNotWaiting(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := ApplyPolicy(ctx, st, &Policy{Roles: []Role{{Name: "boss", Permissions: []string{"access:manage"}}},
		Bindings: []Binding{{Kind: "subject", Role: "boss", Issuer: issuerA, Subject: "boss"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := InviteUser(ctx, st, "gone@example.com", []string{"boss"}, ""); err != nil {
		t.Fatal(err)
	}
	if left, err := CountUnbound(ctx, st.DB); err != nil || left.Invitations != 1 {
		t.Fatalf("before removing: %+v %v", left, err)
	}
	if _, err := RemoveUser(ctx, st, "gone@example.com"); err != nil {
		t.Fatal(err)
	}
	if left, err := CountUnbound(ctx, st.DB); err != nil || left.Total() != 0 {
		t.Errorf("after removing: %+v %v", left, err)
	}
	if n, err := AdoptBindingIssuers(ctx, st, direct(t, issuerA)); err != nil || n != 0 {
		t.Errorf("adopted %d: %v", n, err)
	}
}

// A disabled or removed address stays out through every provider and every binding, and
// test-policy says what a real sign-in would get.
func TestRefusedAddressStaysOutThroughAnyProvider(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	chat := "chat"
	if err := ApplyPolicy(ctx, st, &Policy{Roles: []Role{{Name: "boss", Permissions: []string{"access:manage"}}, {Name: "chat", Permissions: []string{"inference:chat"}}},
		Bindings:    []Binding{{Kind: "email", Role: "boss", Email: "gone@example.com", Issuer: issuerA}, {Kind: "email", Role: "boss", Email: "off@example.com", Issuer: issuerA}},
		DefaultRole: &chat}); err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{"gone@example.com", "off@example.com"} {
		if _, err := InviteUser(ctx, st, email, []string{"chat"}, issuerA); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := RemoveUser(ctx, st, "gone@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := SetUserStatus(ctx, st, "off@example.com", "disabled"); err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{"gone@example.com", "off@example.com"} {
		for _, issuer := range []string{issuerA, issuerB} {
			id := Identity{Issuer: issuer, Subject: "s-" + email, VerifiedEmail: email}
			for _, claim := range []bool{false, true} {
				if got := decision(t, st, id, claim); len(got) != 0 {
					t.Errorf("%s through %s (claim %v): %v", email, issuer, claim, got)
				}
			}
		}
	}
	// Someone with no record still gets the default role, from any provider.
	if got := decision(t, st, Identity{Issuer: issuerB, Subject: "new", VerifiedEmail: "new@example.com"}, true); strings.Join(got, ",") != "inference:chat" {
		t.Errorf("a stranger with a default role: %v", got)
	}
}

func TestMagicLinkOnlyForItsOwnPeople(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	magic := MagicIssuer(testOrigin)
	if err := ApplyPolicy(ctx, st, &Policy{Roles: []Role{{Name: "boss", Permissions: []string{"access:manage"}}, {Name: "chat", Permissions: []string{"inference:chat"}}},
		Bindings: []Binding{{Kind: "subject", Role: "boss", Issuer: issuerA, Subject: "boss"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := InviteUser(ctx, st, "mine@example.com", []string{"chat"}, magic); err != nil {
		t.Fatal(err)
	}
	if _, err := InviteUser(ctx, st, "theirs@example.com", []string{"chat"}, issuerA); err != nil {
		t.Fatal(err)
	}
	if token, _, _ := IssueMagicLink(ctx, st, "theirs@example.com", magic); token != "" {
		t.Error("a link went to someone invited through another provider")
	}
	token, _, err := IssueMagicLink(ctx, st, "mine@example.com", magic)
	if err != nil || token == "" {
		t.Fatalf("no link for someone invited for it: %v", err)
	}
	id, err := ConsumeMagicLink(ctx, st, token, magic)
	if err != nil || id == nil {
		t.Fatalf("redeem: %v", err)
	}
	if got := decision(t, st, *id, true); strings.Join(got, ",") != "inference:chat" {
		t.Errorf("after a magic link: %v", got)
	}
	// Someone already linked through it gets a link, whatever their invitation said.
	if _, err := st.DB.Exec(`DELETE FROM go_invitation_issuers`); err != nil {
		t.Fatal(err)
	}
	if token, _, err := IssueMagicLink(ctx, st, "mine@example.com", magic); err != nil {
		t.Fatal(err)
	} else if token == "" {
		// The per-minute limit counts: the earlier token was consumed, so none is pending.
		t.Error("no link for someone already linked through this provider")
	}
}

func TestBindIssuersNeverGuesses(t *testing.T) {
	policy := func() *Policy {
		return &Policy{Roles: []Role{{Name: "a", Permissions: []string{"access:manage"}}}, Bindings: []Binding{
			{Kind: "email", Role: "a", Email: "me@example.com"},
			{Kind: "subject", Role: "a", Issuer: "https://elsewhere.example.com", Subject: "s"},
			{Kind: "email", Role: "a", Email: "named@example.com", Issuer: issuerB},
		}}
	}
	// One provider: nothing to guess. A named binding stays as named, a subject as it is.
	p := policy()
	if err := p.BindIssuers([]string{issuerA}, nil); err != nil {
		t.Fatal(err)
	}
	if p.Bindings[0].Issuer != issuerA || p.Bindings[1].Issuer != "https://elsewhere.example.com" || p.Bindings[2].Issuer != issuerB {
		t.Errorf("one provider: %+v", p.Bindings)
	}
	// Several: refused, naming them.
	p = policy()
	err := p.BindIssuers([]string{issuerA, issuerB}, nil)
	var ae *AccessError
	if err == nil || !asAccess(err, &ae) || ae.Code != "issuer-required" || !strings.Contains(ae.Message, issuerA) || !strings.Contains(ae.Message, issuerB) || !strings.Contains(ae.Message, "me@example.com") {
		t.Errorf("several providers: %v", err)
	}
	// Several, for your own address: the provider you signed in with.
	p = policy()
	me := &Identity{Issuer: issuerB, Subject: "me", VerifiedEmail: "ME@example.com"}
	if err := p.BindIssuers([]string{issuerA, issuerB}, me); err != nil || p.Bindings[0].Issuer != issuerB {
		t.Errorf("your own address: %v %+v", err, p.Bindings[0])
	}
	// But not if that provider is no longer configured, and not for someone else's.
	p = policy()
	if err := p.BindIssuers([]string{issuerA, "https://c.example.com"}, me); err == nil {
		t.Error("a caller's own provider that isn't configured was used")
	}
	p = policy()
	p.Bindings[0].Email = "other@example.com"
	if err := p.BindIssuers([]string{issuerA, issuerB}, me); err == nil {
		t.Error("a caller's provider was used for someone else's address")
	}
	// No provider: left without a name, which matches nobody.
	p = policy()
	if err := p.BindIssuers(nil, nil); err != nil || p.Bindings[0].Issuer != "" {
		t.Errorf("no provider: %v %+v", err, p.Bindings[0])
	}
}

func TestInviteIssuerRules(t *testing.T) {
	for _, tc := range []struct {
		name    string
		named   string
		issuers []string
		want    string
		refused string
	}{
		{"named and configured", issuerB, []string{issuerA, issuerB}, issuerB, ""},
		{"named, not configured", "https://c.example.com", []string{issuerA, issuerB}, "", "issuer-unknown"},
		{"not named, one provider", "", []string{issuerA}, issuerA, ""},
		{"not named, several", "", []string{issuerA, issuerB}, "", "issuer-required"},
		{"no provider", "", nil, "", ""},
		{"named, no provider", issuerA, nil, issuerA, ""},
	} {
		got, err := InviteIssuer(tc.named, "x@example.com", tc.issuers, nil)
		var ae *AccessError
		switch {
		case tc.refused != "" && (err == nil || !asAccess(err, &ae) || ae.Code != tc.refused):
			t.Errorf("%s: %v", tc.name, err)
		case tc.refused == "" && (err != nil || got != tc.want):
			t.Errorf("%s: %q %v", tc.name, got, err)
		}
	}
}

// oldRows writes what a version that didn't know about providers leaves behind.
func oldRows(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	if err := ApplyPolicy(ctx, st, &Policy{Roles: []Role{{Name: "owner", Permissions: Permissions}, {Name: "chat", Permissions: []string{"inference:chat"}}},
		Bindings: []Binding{{Kind: "email", Role: "owner", Email: "owner@example.com"}, {Kind: "email-domain", Role: "chat", Domain: "example.com"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := InviteUser(ctx, st, "pending@example.com", []string{"chat"}, ""); err != nil {
		t.Fatal(err)
	}
}

// What an existing root has when it first starts on this version: one provider, an email
// binding and an owner. It must keep working with that provider, and only with it.
func TestUpgradeWithOneProviderKeepsTheOwnerSignedIn(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	oldRows(t, st)
	const google = "https://accounts.google.com"
	owner := Identity{Issuer: google, Subject: "owner", VerifiedEmail: "owner@example.com"}
	// Until adopted, an old binding matches nobody.
	if got := decision(t, st, owner, false); len(got) != 0 {
		t.Fatalf("an old binding before adoption: %v", got)
	}
	n, err := AdoptBindingIssuers(ctx, st, direct(t, google))
	if err != nil || n != 3 {
		t.Fatalf("adopted %d: %v", n, err)
	}
	if got := decision(t, st, owner, false); len(got) != len(Permissions) {
		t.Errorf("the owner after adoption: %v", got)
	}
	if got := decision(t, st, Identity{Issuer: "https://other.example.com", Subject: "x", VerifiedEmail: "owner@example.com"}, false); len(got) != 0 {
		t.Errorf("another provider after adoption: %v", got)
	}
	users, _ := ListUsers(ctx, st)
	if users[0].Issuer != google {
		t.Errorf("the invitation: %+v", users[0])
	}
	// It happens once: a second start changes nothing.
	if n, err := AdoptBindingIssuers(ctx, st, direct(t, google)); err != nil || n != 0 {
		t.Errorf("a second start adopted %d: %v", n, err)
	}
	// A later provider doesn't inherit anything already named.
	if n, err := AdoptBindingIssuers(ctx, st, direct(t, google, issuerA)); err != nil || n != 0 {
		t.Errorf("adding a provider: %d %v", n, err)
	}
	if got := decision(t, st, Identity{Issuer: issuerA, Subject: "x", VerifiedEmail: "owner@example.com"}, false); len(got) != 0 {
		t.Errorf("a provider added later: %v", got)
	}
}

func TestUpgradeWithSeveralProvidersDoesNotGuess(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	oldRows(t, st)
	cfg := direct(t, issuerA, issuerB)
	n, err := AdoptBindingIssuers(ctx, st, cfg)
	if err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
	var named int
	st.DB.QueryRow(`SELECT (SELECT COUNT(*) FROM go_binding_issuers) + (SELECT COUNT(*) FROM go_invitation_issuers)`).Scan(&named)
	if named != 0 {
		t.Errorf("a guess wrote %d names", named)
	}
	// Those rows grant nothing, through either provider, and the count says so.
	for _, issuer := range []string{issuerA, issuerB} {
		if got := decision(t, st, Identity{Issuer: issuer, Subject: "o", VerifiedEmail: "owner@example.com"}, true); len(got) != 0 {
			t.Errorf("an unnamed binding through %s: %v", issuer, got)
		}
		if got := decision(t, st, Identity{Issuer: issuer, Subject: "d", VerifiedEmail: "x@example.com"}, true); len(got) != 0 {
			t.Errorf("an unnamed domain binding through %s: %v", issuer, got)
		}
		if got := decision(t, st, Identity{Issuer: issuer, Subject: "p", VerifiedEmail: "pending@example.com"}, true); len(got) != 0 {
			t.Errorf("an unnamed invitation through %s: %v", issuer, got)
		}
	}
	left, err := CountUnbound(ctx, st.DB)
	if err != nil || left != (UnboundCount{Emails: 1, Domains: 1, Invitations: 1}) {
		t.Fatalf("left: %+v %v", left, err)
	}
	for _, want := range []string{"3 bindings and invitations", "local-base access bindings adopt --issuer <issuer>", "grant nothing"} {
		if !strings.Contains(left.Notice(), want) {
			t.Errorf("notice lacks %q: %s", want, left.Notice())
		}
	}
	// The answer is one command, for a provider that exists.
	if _, err := AdoptBindingIssuer(ctx, st, cfg, "https://c.example.com"); err == nil {
		t.Error("adopted a provider that isn't configured")
	}
	if n, err := AdoptBindingIssuer(ctx, st, cfg, issuerB); err != nil || n != 3 {
		t.Fatalf("adopt: %d %v", n, err)
	}
	if left, err := CountUnbound(ctx, st.DB); err != nil || left.Total() != 0 || left.Notice() != "" {
		t.Errorf("after choosing: %+v %q %v", left, left.Notice(), err)
	}
	if got := decision(t, st, Identity{Issuer: issuerB, Subject: "o", VerifiedEmail: "owner@example.com"}, false); len(got) != len(Permissions) {
		t.Errorf("after choosing: %v", got)
	}
	if n, err := AdoptBindingIssuers(ctx, st, cfg); err != nil || n != 0 {
		t.Errorf("then starting: %d %v", n, err)
	}
}

func TestUpgradeWithNoProviderWaits(t *testing.T) {
	st := testStore(t)
	oldRows(t, st)
	if n, err := AdoptBindingIssuers(context.Background(), st, nil); err != nil || n != 0 {
		t.Errorf("no configuration: %d %v", n, err)
	}
	if left, err := CountUnbound(context.Background(), st.DB); err != nil || left.Total() != 3 {
		t.Errorf("left: %+v %v", left, err)
	}
	onlyOld := &BrowserAccess{Origin: testOrigin, Provider: Provider{Kind: "direct", Registrations: []Registration{{Kind: "github-oauth", ID: "gh", Name: "GitHub"}}}}
	if n, err := AdoptBindingIssuers(context.Background(), st, onlyOld); err != nil || n != 0 {
		t.Errorf("a provider that can't sign in: %d %v", n, err)
	}
}

// Someone already linked keeps their provider, without a choice being needed for them.
func TestUpgradeKeepsPeopleWithTheirProvider(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := ApplyPolicy(ctx, st, &Policy{Roles: []Role{{Name: "boss", Permissions: []string{"access:manage"}}, {Name: "chat", Permissions: []string{"inference:chat"}}},
		Bindings: []Binding{{Kind: "subject", Role: "boss", Issuer: issuerA, Subject: "boss"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := InviteUser(ctx, st, "linked@example.com", []string{"chat"}, issuerB); err != nil {
		t.Fatal(err)
	}
	linked := Identity{Issuer: issuerB, Subject: "linked", VerifiedEmail: "linked@example.com"}
	decision(t, st, linked, true)
	if _, err := st.DB.Exec(`DELETE FROM go_invitation_issuers`); err != nil { // as an older version leaves it
		t.Fatal(err)
	}
	// They aren't counted as waiting for a choice, because nobody has to make one.
	if left, err := CountUnbound(ctx, st.DB); err != nil || left.Total() != 0 {
		t.Errorf("left: %+v %v", left, err)
	}
	// Several providers, and nothing to guess.
	n, err := AdoptBindingIssuers(ctx, st, direct(t, issuerA, issuerB))
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	users, _ := ListUsers(ctx, st)
	if users[0].Issuer != issuerB {
		t.Errorf("%+v", users[0])
	}
	if got := decision(t, st, linked, true); strings.Join(got, ",") != "inference:chat" {
		t.Errorf("after adoption: %v", got)
	}
}

// A rollback writes bindings with no provider. The next start names them.
func TestUpgradeAgainAfterARollback(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	oldRows(t, st)
	cfg := direct(t, issuerA)
	if _, err := AdoptBindingIssuers(ctx, st, cfg); err != nil {
		t.Fatal(err)
	}
	var role string
	st.DB.QueryRow(`SELECT id FROM auth_roles WHERE name = 'owner'`).Scan(&role)
	if _, err := st.DB.Exec(`INSERT INTO auth_email_role_bindings (id, role_id, email) VALUES ('rolled', ?, 'later@example.com')`, role); err != nil {
		t.Fatal(err)
	}
	if n, err := AdoptBindingIssuers(ctx, st, cfg); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if got := decision(t, st, Identity{Issuer: issuerA, Subject: "l", VerifiedEmail: "later@example.com"}, false); len(got) != len(Permissions) {
		t.Errorf("a binding from a rollback: %v", got)
	}
}

// The lockout rules decide from a model of sign-in. The model must agree with sign-in.
func TestGuardAgreesWithSignIn(t *testing.T) {
	const manage = "access:manage"
	chatRole, bossRole := Role{Name: "chat", Permissions: []string{"inference:chat"}}, Role{Name: "boss", Permissions: []string{manage}}
	boss := "boss"
	identities := map[string]Identity{
		"right provider, bound email":  {Issuer: issuerA, Subject: "1", VerifiedEmail: "bound@example.com"},
		"other provider, bound email":  {Issuer: issuerB, Subject: "2", VerifiedEmail: "bound@example.com"},
		"right provider, bound domain": {Issuer: issuerA, Subject: "3", VerifiedEmail: "x@domain.example"},
		"other provider, bound domain": {Issuer: issuerB, Subject: "4", VerifiedEmail: "x@domain.example"},
		"right provider, subject":      {Issuer: issuerA, Subject: "subject-bound"},
		"other provider, subject":      {Issuer: issuerB, Subject: "subject-bound"},
		"right provider, invited":      {Issuer: issuerA, Subject: "5", VerifiedEmail: "invited@example.com"},
		"other provider, invited":      {Issuer: issuerB, Subject: "6", VerifiedEmail: "invited@example.com"},
		"right provider, removed":      {Issuer: issuerA, Subject: "7", VerifiedEmail: "removed@example.com"},
		"other provider, removed":      {Issuer: issuerB, Subject: "8", VerifiedEmail: "removed@example.com"},
		"right provider, disabled":     {Issuer: issuerA, Subject: "9", VerifiedEmail: "off@example.com"},
		"other provider, disabled":     {Issuer: issuerB, Subject: "10", VerifiedEmail: "off@example.com"},
		"stranger":                     {Issuer: issuerA, Subject: "11", VerifiedEmail: "stranger@example.org"},
		"stranger, no email":           {Issuer: issuerB, Subject: "12"},
		"provider not configured":      {Issuer: "https://c.example.com", Subject: "13", VerifiedEmail: "bound@example.com"},
	}
	for name, withDefault := range map[string]*string{"no default role": nil, "a managing default role": &boss} {
		t.Run(name, func(t *testing.T) {
			st := testStore(t)
			ctx := context.Background()
			if err := ApplyPolicy(ctx, st, &Policy{Roles: []Role{chatRole, bossRole}, DefaultRole: withDefault, Bindings: []Binding{
				{Kind: "email", Role: "boss", Email: "bound@example.com", Issuer: issuerA},
				{Kind: "email-domain", Role: "boss", Domain: "domain.example", Issuer: issuerA},
				{Kind: "subject", Role: "boss", Issuer: issuerA, Subject: "subject-bound"},
				{Kind: "email", Role: "boss", Email: "removed@example.com", Issuer: issuerA},
				{Kind: "email", Role: "boss", Email: "off@example.com", Issuer: issuerA},
			}}); err != nil {
				t.Fatal(err)
			}
			for _, email := range []string{"invited@example.com", "removed@example.com", "off@example.com"} {
				if _, err := InviteUser(ctx, st, email, []string{"boss"}, issuerA); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := RemoveUser(ctx, st, "removed@example.com"); err != nil {
				t.Fatal(err)
			}
			if _, _, err := SetUserStatus(ctx, st, "off@example.com", "disabled"); err != nil {
				t.Fatal(err)
			}
			g, err := LoadGuard(ctx, st, direct(t, issuerA, issuerB))
			if err != nil {
				t.Fatal(err)
			}
			for name, id := range identities {
				want := slices.Contains(decision(t, st, id, true), manage)
				if id.Issuer == "https://c.example.com" {
					want = false // the sign-in itself can't happen
				}
				if got := g.CanManage(id); got != want {
					t.Errorf("%s: the guard says %v, sign-in says %v", name, got, want)
				}
			}
		})
	}
}

// Whoever is left must actually be able to sign in.
func TestHasManagerCountsOnlyWhoCanSignIn(t *testing.T) {
	boss := Role{Name: "boss", Permissions: []string{"access:manage"}}
	for _, tc := range []struct {
		name string
		g    Guard
		want bool
	}{
		{"a key, no providers", Guard{KeyManagers: []string{"k"}}, true},
		{"no provider, provider-wide", Guard{ProviderPerms: []string{"access:manage"}}, false},
		{"a provider, provider-wide", Guard{ProviderPerms: []string{"access:manage"}, Issuers: []string{issuerA}}, true},
		{"binding on a configured provider", Guard{Issuers: []string{issuerA}, Policy: &Policy{Roles: []Role{boss}, Bindings: []Binding{{Kind: "email", Role: "boss", Email: "a@example.com", Issuer: issuerA}}}}, true},
		{"binding on a removed provider", Guard{Issuers: []string{issuerB}, Policy: &Policy{Roles: []Role{boss}, Bindings: []Binding{{Kind: "email", Role: "boss", Email: "a@example.com", Issuer: issuerA}}}}, false},
		{"binding with no provider", Guard{Issuers: []string{issuerB}, Policy: &Policy{Roles: []Role{boss}, Bindings: []Binding{{Kind: "email", Role: "boss", Email: "a@example.com"}}}}, false},
		{"domain on a removed provider", Guard{Issuers: []string{issuerB}, Policy: &Policy{Roles: []Role{boss}, Bindings: []Binding{{Kind: "email-domain", Role: "boss", Domain: "example.com", Issuer: issuerA}}}}, false},
		{"subject on a removed provider", Guard{Issuers: []string{issuerB}, Policy: &Policy{Roles: []Role{boss}, Bindings: []Binding{{Kind: "subject", Role: "boss", Issuer: issuerA, Subject: "s"}}}}, false},
		{"bound person removed", Guard{Issuers: []string{issuerA}, Users: []GuardUser{{Email: "a@example.com", Status: "removed"}}, Policy: &Policy{Roles: []Role{boss}, Bindings: []Binding{{Kind: "email", Role: "boss", Email: "a@example.com", Issuer: issuerA}}}}, false},
		{"person with a role, right provider", Guard{Issuers: []string{issuerA}, Users: []GuardUser{{Email: "a@example.com", Status: "active", Roles: []string{"boss"}, Issuer: issuerA}}, Policy: &Policy{Roles: []Role{boss}}}, true},
		{"person with a role, removed provider", Guard{Issuers: []string{issuerB}, Users: []GuardUser{{Email: "a@example.com", Status: "active", Roles: []string{"boss"}, Issuer: issuerA}}, Policy: &Policy{Roles: []Role{boss}}}, false},
		{"person linked through a configured provider", Guard{Issuers: []string{issuerB}, Users: []GuardUser{{Email: "a@example.com", Status: "active", Roles: []string{"boss"}, Identities: [][2]string{{issuerB, "s"}}}}, Policy: &Policy{Roles: []Role{boss}}}, true},
	} {
		if got := tc.g.HasManager(); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
}

func asAccess(err error, target **AccessError) bool { return errors.As(err, target) }
