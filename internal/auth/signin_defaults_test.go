package auth

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// signIn runs the real sign-in flow for an identity the fake provider vouches for.
func signIn(t *testing.T, st *store.Store, perms []string, claims map[string]any) (*fakeIdP, *Browser, *http.Response) {
	t.Helper()
	f := newFakeIdP(t)
	f.claims = claims
	reg := Registration{Kind: "oidc", ID: "idp", Name: "IdP", Issuer: f.srv.URL, ClientID: "cid",
		ClientAuthentication: &ClientAuth{Kind: "client-secret-basic", ClientSecret: "shh"}}
	b := browserFor(t, st, reg)
	b.Config.Permissions = perms
	b.Client = f.srv.Client()
	return f, b, nil
}

func sessions(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	st.DB.QueryRow(`SELECT COUNT(*) FROM auth_browser_sessions`).Scan(&n)
	return n
}

// Someone a policy gives nothing gets no session, and is told so. Their address isn't kept.
func TestNoSessionForASignInWithNoPermissions(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	f, b, _ := signIn(t, st, []string{}, map[string]any{"email": "stranger@example.org", "sub": "s1", "email_verified": true})
	if err := ApplyPolicy(ctx, st, &Policy{Roles: []Role{{Name: "owner", Permissions: Permissions}},
		Bindings: []Binding{{Kind: "email", Role: "owner", Email: "owner@example.org", Issuer: f.srv.URL}}}); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	b.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	resp := login(t, b, f, nil)
	if cookieNamed(resp, SessionCookie) != nil {
		t.Error("a session cookie for someone with no permissions")
	}
	if sessions(t, st) != 0 {
		t.Error("a session row for someone with no permissions")
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "no access") {
		t.Errorf("%d: %s", resp.StatusCode, body)
	}
	// The way on is there for whoever runs this LocalBase, with this address filled in.
	if !strings.Contains(body, "local-base access users invite --email stranger@example.org --roles") {
		t.Errorf("the page doesn't say what to run: %s", body)
	}
	if !strings.Contains(body, "You signed in as stranger@example.org") {
		t.Errorf("the page doesn't say who: %s", body)
	}
	if strings.Contains(body, "policy starter") {
		t.Errorf("the page offers two next steps: %s", body)
	}
	if st := cookieNamed(resp, loginStateCookie); st == nil || st.MaxAge >= 0 {
		t.Errorf("the state cookie stays: %+v", st)
	}
	if !strings.Contains(logs.String(), `"event":"auth.signin-denied"`) {
		t.Errorf("not on record: %s", logs.String())
	}
	if strings.Contains(logs.String(), "stranger@example.org") {
		t.Errorf("the log holds the address: %s", logs.String())
	}
}

// The page repeats what a provider sent, so it must not be able to write markup.
func TestDeniedPageEscapesTheAddress(t *testing.T) {
	st := testStore(t)
	f, b, _ := signIn(t, st, []string{}, map[string]any{"email": `"<i>x</i>"@example.org`, "sub": "s1", "email_verified": true})
	resp := login(t, b, f, nil)
	if body := readBody(t, resp); strings.Contains(body, "<i>") {
		t.Errorf("markup got through: %s", body)
	}
	// With no address to show, it still says what to do.
	st2 := testStore(t)
	f2, b2, _ := signIn(t, st2, []string{}, map[string]any{"email": nil, "email_verified": nil, "sub": "s2"})
	resp2 := login(t, b2, f2, nil)
	if body := readBody(t, resp2); resp2.StatusCode != http.StatusForbidden || !strings.Contains(body, "local-base access policy starter --email") {
		t.Errorf("%d: %s", resp2.StatusCode, body)
	}
}

// An address or an issuer can hold anything a provider sends, so the page escapes both,
// whichever command it shows.
func TestDeniedPageEscapesWhatItQuotes(t *testing.T) {
	for _, hasPolicy := range []bool{false, true} {
		page := noAccessPage(`"<i>x</i>"@example.org`, `https://example.org/<b>`, hasPolicy)
		for _, markup := range []string{"<i>", "</i>", "<b>"} {
			if strings.Contains(page, markup) {
				t.Errorf("policy %v: %q got through: %s", hasPolicy, markup, page)
			}
		}
		if !strings.Contains(page, "&lt;i&gt;") || !strings.Contains(page, "&lt;b&gt;") {
			t.Errorf("policy %v: the quoted values are gone: %s", hasPolicy, page)
		}
	}
}

// With several providers the one command has to say which, so it works as printed.
func TestDeniedPageNamesTheProviderWhenThereAreSeveral(t *testing.T) {
	st := testStore(t)
	f, b, _ := signIn(t, st, []string{}, map[string]any{"email": "owner@example.org", "sub": "o", "email_verified": true})
	b.Config.Provider.Registrations = append(b.Config.Provider.Registrations, Registration{Kind: "oidc", ID: "other", Name: "Other", Issuer: "https://other.example.org", ClientID: "c", ClientAuthentication: &ClientAuth{Kind: "none"}})
	w := httptest.NewRecorder()
	b.establish(w, get(OIDCCallbackPath), Identity{Issuer: f.srv.URL, Subject: "o", VerifiedEmail: "owner@example.org"}, "browser:o")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "--email owner@example.org --issuer "+f.srv.URL) {
		t.Errorf("%d: %s", w.Code, w.Body)
	}
}

// The owner's own first sign-in, on a root with a binding for them.
func TestOwnersFirstSignInWithABinding(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	f, b, _ := signIn(t, st, []string{}, map[string]any{"email": "owner@example.org", "sub": "owner", "email_verified": true})
	if err := ApplyPolicy(ctx, st, StarterPolicy("owner@example.org", f.srv.URL)); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	b.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	resp := login(t, b, f, nil)
	session := cookieNamed(resp, SessionCookie)
	if session == nil || resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("no session: %d", resp.StatusCode)
	}
	id, owner := b.Authenticate(get("/app/session", session))
	if d := b.Authorize(ctx, id); len(d.Permissions) != len(Permissions) || strings.Join(d.MatchedRoles, ",") != "admin" {
		t.Errorf("%+v", d)
	}
	for _, want := range []string{`"event":"auth.signin"`, `"actor_id":"` + owner + `"`, `"roles":"admin"`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log lacks %s: %s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), "owner@example.org") {
		t.Errorf("the log holds the address: %s", logs.String())
	}
}

// The owner's first sign-in on a root with nothing yet: no session, and the page gives them
// the command that fixes it. Running it makes the same sign-in work.
func TestOwnersFirstSignInWithNothingSetUp(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	f, b, _ := signIn(t, st, []string{}, map[string]any{"email": "owner@example.org", "sub": "owner", "email_verified": true})
	resp := login(t, b, f, nil)
	if cookieNamed(resp, SessionCookie) != nil {
		t.Fatal("a session with no permissions anywhere")
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "local-base access policy starter --email owner@example.org") {
		t.Error("no way on")
	}
	if strings.Contains(body, "users invite") || strings.Contains(body, "--issuer") {
		t.Errorf("more than the one next step: %s", body)
	}
	if err := ApplyPolicy(ctx, st, StarterPolicy("owner@example.org", f.srv.URL)); err != nil {
		t.Fatal(err)
	}
	resp = login(t, b, f, nil)
	session := cookieNamed(resp, SessionCookie)
	if session == nil {
		t.Fatalf("after the command: %d", resp.StatusCode)
	}
	id, _ := b.Authenticate(get("/app/session", session))
	if d := b.Authorize(ctx, id); len(d.Permissions) != len(Permissions) {
		t.Errorf("%+v", d)
	}
}

// Provider-wide permissions someone set on purpose still let people in.
func TestProviderWidePermissionsSetOnPurposeStillWork(t *testing.T) {
	st := testStore(t)
	f, b, _ := signIn(t, st, []string{"inference:chat"}, map[string]any{"email": "anyone@example.org", "sub": "a", "email_verified": true})
	if resp := login(t, b, f, nil); cookieNamed(resp, SessionCookie) == nil {
		t.Errorf("no session: %d", resp.StatusCode)
	}
}

// An invitation is claimed on first sign-in, so its permissions count for that sign-in.
func TestInvitedPersonsFirstSignIn(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	f, b, _ := signIn(t, st, []string{}, map[string]any{"email": "friend@example.org", "sub": "f", "email_verified": true})
	if err := ApplyPolicy(ctx, st, &Policy{Roles: []Role{{Name: "boss", Permissions: []string{"access:manage"}}, {Name: "chat", Permissions: []string{"inference:chat"}}},
		Bindings: []Binding{{Kind: "subject", Role: "boss", Issuer: f.srv.URL, Subject: "boss"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := InviteUser(ctx, st, "friend@example.org", []string{"chat"}, f.srv.URL); err != nil {
		t.Fatal(err)
	}
	if resp := login(t, b, f, nil); cookieNamed(resp, SessionCookie) == nil {
		t.Errorf("an invited person: %d", resp.StatusCode)
	}
}

// New installs give nothing to whoever happens to sign in.
func TestNewInstallDefaults(t *testing.T) {
	if len(DefaultBrowserPermissions) != 0 {
		t.Errorf("provider-wide permissions by default: %v", DefaultBrowserPermissions)
	}
	p := StarterPolicy("Owner@Example.org", "https://idp.example.org")
	if err := p.Normalize(); err != nil {
		t.Fatal(err)
	}
	if p.DefaultRole != nil {
		t.Errorf("a default role: %q", *p.DefaultRole)
	}
	if len(p.Bindings) != 1 || p.Bindings[0].Email != "owner@example.org" || p.Bindings[0].Issuer != "https://idp.example.org" || p.Bindings[0].Role != "admin" {
		t.Errorf("bindings: %+v", p.Bindings)
	}
	names := []string{}
	for _, r := range p.Roles {
		names = append(names, r.Name)
		if r.Name == "admin" && len(r.Permissions) != len(Permissions) {
			t.Errorf("admin holds %v", r.Permissions)
		}
	}
	if !slices.Contains(names, "admin") || !slices.Contains(names, "member") {
		t.Errorf("roles %v", names)
	}
}

func TestOpenSignIn(t *testing.T) {
	oidc := func(names ...string) *BrowserAccess {
		cfg := &BrowserAccess{Origin: testOrigin, Provider: Provider{Kind: "direct"}}
		for _, n := range names {
			cfg.Provider.Registrations = append(cfg.Provider.Registrations, Registration{Kind: "oidc", ID: strings.ToLower(n), Name: n, Issuer: "https://" + strings.ToLower(n) + ".example.org", ClientID: "c", ClientAuthentication: &ClientAuth{Kind: "none"}})
		}
		return cfg
	}
	chat := "chat"
	withDefault := &Policy{Roles: []Role{{Name: "chat", Permissions: []string{"inference:chat"}}}, DefaultRole: &chat}
	noDefault := &Policy{Roles: []Role{{Name: "chat", Permissions: []string{"inference:chat"}}}}
	for _, tc := range []struct {
		name   string
		cfg    *BrowserAccess
		policy *Policy
		perms  []string
		want   string
	}{
		{"no provider", nil, withDefault, nil, ""},
		{"a default role", oidc("Google"), withDefault, nil, "Anyone who can sign in through Google gets: inference:chat."},
		{"two providers", oidc("Google", "Okta"), withDefault, nil, "Anyone who can sign in through Google or Okta gets: inference:chat."},
		{"a policy with no default", oidc("Google"), noDefault, []string{"models:manage"}, ""},
		{"no policy, permissions set", oidc("Google"), nil, []string{"inference:chat", "models:read"}, "Anyone who can sign in through Google gets: inference:chat, models:read."},
		{"no policy, none set", oidc("Google"), nil, []string{}, ""},
		{"a default role with nothing in it", oidc("Google"), &Policy{Roles: []Role{{Name: "chat"}}, DefaultRole: &chat}, nil, ""},
		{"Cloudflare Access", &BrowserAccess{Provider: Provider{Kind: "cloudflare-access", TeamDomain: "t.cloudflareaccess.com", Audience: "a"}, Origin: testOrigin}, withDefault, nil, "Anyone who can sign in through Cloudflare Access gets: inference:chat."},
		{"a provider that can't sign in", &BrowserAccess{Provider: Provider{Kind: "direct", Registrations: []Registration{{Kind: "github-oauth", ID: "gh", Name: "GitHub"}}}, Origin: testOrigin}, withDefault, nil, ""},
	} {
		cfg := tc.cfg
		if cfg != nil {
			cfg.Permissions = tc.perms
		}
		if got := OpenSignIn(cfg, tc.policy); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	magic := &BrowserAccess{Origin: testOrigin, Permissions: []string{"inference:chat"}, Provider: Provider{Kind: "direct", Registrations: []Registration{{Kind: "magic-link", ID: "email", Name: "Email"}}}}
	if got := OpenSignIn(magic, nil); !strings.Contains(got, "through Email gets") {
		t.Errorf("magic link: %q", got)
	}
}

// The access page shows who has signed in: one line a person, the latest first.
func TestSignedInLists(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	id := func(sub, email string) Identity {
		return Identity{Issuer: "https://idp.example.org", Subject: sub, VerifiedEmail: email}
	}
	for _, s := range []struct {
		owner string
		id    Identity
	}{{"browser:a", id("a", "a@example.org")}, {"browser:b", id("b", "")}, {"browser:a", id("a", "a@example.org")}} {
		if _, err := CreateSession(ctx, st, s.owner, s.id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB.Exec(`UPDATE auth_browser_sessions SET last_seen_at = 5000 WHERE owner_id = 'browser:b'`); err != nil {
		t.Fatal(err)
	}
	got, err := SignedIn(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Email != "a@example.org" || got[1].Email != "" || got[1].Issuer != "https://idp.example.org" || got[0].LastSeen == "" {
		t.Errorf("%+v", got)
	}
	// An expired session isn't someone signed in.
	if _, err := st.DB.Exec(`UPDATE auth_browser_sessions SET idle_expires_at = 1 WHERE owner_id = 'browser:a'`); err != nil {
		t.Fatal(err)
	}
	if got, err := SignedIn(ctx, st); err != nil || len(got) != 1 {
		t.Errorf("after expiry: %+v %v", got, err)
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
