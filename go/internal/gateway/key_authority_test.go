package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// keyFixture is a gateway with real keys, called with real credentials.
type keyFixture struct {
	t    *testing.T
	s    *Server
	st   *store.Store
	logs *bytes.Buffer
}

func newKeyFixture(t *testing.T) *keyFixture {
	t.Helper()
	r, err := root.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	logs := &bytes.Buffer{}
	s := New(&Server{Store: st, Root: r, Auth: &auth.Resolver{Store: st, Mode: "either"}, Log: slog.New(slog.NewJSONHandler(logs, nil))})
	return &keyFixture{t, s, st, logs}
}

// key makes a key directly in the store and returns its record and secret.
func (f *keyFixture) key(name string, days int, scopes ...string) (*store.APIKey, string) {
	f.t.Helper()
	k, raw, err := f.st.CreateAPIKeyExpiring(context.Background(), name, scopes, days)
	if err != nil {
		f.t.Fatal(err)
	}
	return k, raw
}

// post sends a request with a bearer credential through the whole server.
func (f *keyFixture) post(secret, path, body string) (int, string, string) {
	f.t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+secret)
	w := httptest.NewRecorder()
	f.s.ServeHTTP(w, r)
	var out struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out.Error.Code, w.Body.String()
}

func (f *keyFixture) expect(secret, body string, status int, code string) string {
	f.t.Helper()
	got, gotCode, out := f.post(secret, "/_localbase/api-keys", body)
	if got != status || gotCode != code {
		f.t.Fatalf("%s: %d %s, want %d %q", body, got, out, status, code)
	}
	return out
}

func (f *keyFixture) stored(id string) *store.APIKey {
	f.t.Helper()
	k, err := f.st.APIKeyByID(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return k
}

func secretOf(t *testing.T, body string) string {
	t.Helper()
	var out struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Secret == "" {
		t.Fatalf("no secret in the response: %v", err)
	}
	return out.Secret
}

func keyAction(action, id string, scopes ...string) string {
	if scopes == nil {
		return `{"action":"` + action + `","keyId":"` + id + `"}`
	}
	return `{"action":"` + action + `","keyId":"` + id + `","scopes":["` + strings.Join(scopes, `","`) + `"]}`
}

const exceeds = "exceeds_caller_permissions"

func TestKeyCreateGrantsOnlyWhatTheCallerHolds(t *testing.T) {
	f := newKeyFixture(t)
	_, narrow := f.key("narrow", 0, "keys:manage", "keys:read", "models:read")
	before, _ := f.st.ListAPIKeys(context.Background())

	out := f.expect(narrow, `{"action":"create","name":"more","scopes":["models:read","access:manage","models:manage"]}`, 403, exceeds)
	// It names what is missing in the caller's terms, and only that.
	if !strings.Contains(out, "access:manage") || !strings.Contains(out, "models:manage") || strings.Contains(out, "models:read") {
		t.Errorf("message: %s", out)
	}
	if after, _ := f.st.ListAPIKeys(context.Background()); len(after) != len(before) {
		t.Errorf("a refused create left a key: %d keys, was %d", len(after), len(before))
	}
	// A key can't copy its own power to something it doesn't hold, even by one scope.
	f.expect(narrow, `{"action":"create","name":"one","scopes":["inference:chat"]}`, 403, exceeds)
	// What it holds, it can hand out, whole or in part.
	f.expect(narrow, `{"action":"create","name":"same","scopes":["keys:manage","keys:read","models:read"]}`, 201, "")
	f.expect(narrow, `{"action":"create","name":"part","scopes":["models:read"]}`, 201, "")
	f.expect(narrow, `{"action":"create","name":"nothing","scopes":[]}`, 201, "")
}

func TestKeySetScopesStaysWithinTheCaller(t *testing.T) {
	f := newKeyFixture(t)
	me, narrow := f.key("narrow", 0, "keys:manage", "keys:read", "models:read")
	small, smallSecret := f.key("small", 0, "models:read")
	wide, _ := f.key("wide", 0, "keys:manage", "keys:read", "models:read", "models:manage")

	// Widening itself is refused, and changes nothing.
	f.expect(narrow, keyAction("set-scopes", me.ID, "keys:manage", "keys:read", "models:read", "access:read"), 403, exceeds)
	if got := f.stored(me.ID).Scopes; slices.Contains(got, "access:read") {
		t.Errorf("widened itself: %v", got)
	}
	// So is widening another key past the caller.
	f.expect(narrow, keyAction("set-scopes", small.ID, "models:read", "access:read"), 403, exceeds)
	if got, _, _ := f.post(smallSecret, "/_localbase/access-management", `{"action":"test-policy"}`); got != 403 {
		t.Errorf("the small key reached access management: %d", got)
	}
	// Changing a key wider than the caller is refused, even to narrow it.
	f.expect(narrow, keyAction("set-scopes", wide.ID, "models:read"), 403, exceeds)
	if got := f.stored(wide.ID).Scopes; !slices.Contains(got, "models:manage") {
		t.Errorf("a wider key changed: %v", got)
	}
	// Within the caller, either way is fine.
	f.expect(narrow, keyAction("set-scopes", small.ID, "models:read", "keys:read"), 200, "")
	f.expect(narrow, keyAction("set-scopes", small.ID), 400, "validation_failed")
}

func TestKeyRotateAndRevokeStayWithinTheCaller(t *testing.T) {
	f := newKeyFixture(t)
	_, narrow := f.key("narrow", 0, "keys:manage", "keys:read")
	owner, ownerSecret := f.key("owner", 0, "keys:manage", "keys:read", "access:manage")
	peer, peerSecret := f.key("peer", 0, "keys:read")
	second, secondSecret := f.key("second", 0, "keys:read")

	// A wider key keeps its secret and stays alive.
	out := f.expect(narrow, keyAction("rotate", owner.ID), 403, exceeds)
	if strings.Contains(out, "access:manage") {
		t.Errorf("the refusal names what the wider key holds: %s", out)
	}
	if strings.Contains(out, `"secret"`) {
		t.Errorf("a refused rotate returned a secret: %s", out)
	}
	f.expect(narrow, keyAction("revoke", owner.ID), 403, exceeds)
	f.expect(ownerSecret, `{"action":"create","name":"proof","scopes":[]}`, 201, "") // the old secret works

	// A key within the caller can be rotated, and the old secret ends there.
	fresh := secretOf(t, f.expect(narrow, keyAction("rotate", peer.ID), 200, ""))
	if got, _, _ := f.post(peerSecret, "/_localbase/api-keys", `{"action":"create","name":"x","scopes":[]}`); got != 401 {
		t.Errorf("the old secret after a rotate: %d", got)
	}
	if fresh == peerSecret {
		t.Error("the rotate returned the same secret")
	}
	f.expect(narrow, keyAction("revoke", second.ID), 200, "")
	if got, _, _ := f.post(secondSecret, "/_localbase/api-keys", `{"action":"create","name":"x","scopes":[]}`); got != 401 {
		t.Errorf("a revoked key: %d", got)
	}
}

func TestKeyLifeIsLimitedByTheCallingKey(t *testing.T) {
	f := newKeyFixture(t)
	short, shortSecret := f.key("short", 10, "keys:manage", "keys:read", "models:read")
	forever, foreverSecret := f.key("forever", 0, "keys:manage", "keys:read", "models:read", "access:read")
	longer, _ := f.key("longer", 30, "models:read")
	sooner, _ := f.key("sooner", 5, "models:read")
	const life = "exceeds_caller_expiry"

	out := f.expect(shortSecret, `{"action":"create","name":"never","scopes":["models:read"]}`, 403, life)
	if !strings.Contains(out, *short.ExpiresAt) || !strings.Contains(out, "can't outlast it") {
		t.Errorf("message: %s", out)
	}
	f.expect(shortSecret, `{"action":"create","name":"later","scopes":["models:read"],"expiresDays":11}`, 403, life)
	f.expect(shortSecret, `{"action":"create","name":"sooner","scopes":["models:read"],"expiresDays":9}`, 201, "")
	// Rotating hands over a key that outlasts the caller; one that doesn't is fine.
	f.expect(shortSecret, keyAction("rotate", longer.ID), 403, life)
	f.expect(shortSecret, keyAction("rotate", sooner.ID), 200, "")
	// Adding to a longer-lived key extends those permissions; removing does not.
	f.expect(shortSecret, keyAction("set-scopes", longer.ID, "models:read", "keys:read"), 403, life)
	f.expect(shortSecret, keyAction("set-scopes", longer.ID), 400, "validation_failed")
	f.expect(shortSecret, `{"action":"set-scopes","keyId":"`+longer.ID+`","scopes":[]}`, 200, "")
	f.expect(shortSecret, keyAction("set-scopes", sooner.ID, "models:read", "keys:read"), 200, "")
	// A revoke gives nothing, so an expiry doesn't limit it.
	f.expect(shortSecret, keyAction("revoke", longer.ID), 200, "")
	// A key's own secret can always be replaced: it doesn't outlast itself.
	if got := secretOf(t, f.expect(shortSecret, keyAction("rotate", short.ID), 200, "")); got == shortSecret {
		t.Error("a rotate kept the old secret")
	}
	if got := f.stored(short.ID).ExpiresAt; got == nil {
		t.Error("a rotate cleared the expiry")
	}
	// A key with no expiry has no such limit, and neither does a person.
	f.expect(foreverSecret, `{"action":"create","name":"never","scopes":["models:read"]}`, 201, "")
	f.expect(foreverSecret, keyAction("rotate", forever.ID), 200, "")
	person := &auth.Principal{Kind: "browser-session", ID: "browser:x", Permissions: []string{"keys:manage", "models:read"}}
	res := f.s.Call(context.Background(), person, "POST", "/_localbase/api-keys", "application/json", []byte(`{"action":"create","name":"p","scopes":["models:read"]}`))
	if res.Status != 201 {
		t.Errorf("a person making a key that never expires: %d %s", res.Status, res.Body)
	}
}

// A person's session goes through the same rules: what it holds is what its role gives.
func TestKeyRulesHoldForASession(t *testing.T) {
	f := newKeyFixture(t)
	wide, _ := f.key("wide", 0, "keys:manage", "access:manage")
	small, _ := f.key("small", 0, "models:read")
	person := &auth.Principal{Kind: "browser-session", ID: "browser:x", Permissions: []string{"keys:manage", "keys:read", "models:read"}}
	call := func(body string) (int, string) {
		res := f.s.Call(context.Background(), person, "POST", "/_localbase/api-keys", "application/json", []byte(body))
		return res.Status, string(res.Body)
	}
	for _, body := range []string{
		`{"action":"create","name":"more","scopes":["access:manage"]}`,
		keyAction("rotate", wide.ID),
		keyAction("revoke", wide.ID),
		keyAction("set-scopes", wide.ID, "models:read"),
		keyAction("set-scopes", small.ID, "models:read", "access:manage"),
	} {
		if status, out := call(body); status != 403 || !strings.Contains(out, exceeds) {
			t.Errorf("%s: %d %s", body, status, out)
		}
	}
	for _, body := range []string{
		`{"action":"create","name":"mine","scopes":["keys:read"]}`,
		keyAction("set-scopes", small.ID, "models:read", "keys:read"),
		keyAction("rotate", small.ID),
		keyAction("revoke", small.ID),
	} {
		if status, out := call(body); status >= 300 {
			t.Errorf("%s: %d %s", body, status, out)
		}
	}
}

// What was true when a request was authenticated may not be true when its turn comes.
func TestKeyChangeUsesWhatTheCallerHoldsNow(t *testing.T) {
	f := newKeyFixture(t)
	k, _ := f.key("caller", 0, "keys:manage", "access:manage")
	other, _ := f.key("other", 0, "keys:read")
	stale := &auth.Principal{Kind: "api-key", ID: k.ID, Permissions: []string{"keys:manage", "access:manage", "models:manage"}}
	call := func(body string) (int, string) {
		res := f.s.Call(context.Background(), stale, "POST", "/_localbase/api-keys", "application/json", []byte(body))
		return res.Status, string(res.Body)
	}
	// It says it holds models:manage. The store says otherwise.
	if status, out := call(`{"action":"create","name":"x","scopes":["models:manage"]}`); status != 403 || !strings.Contains(out, exceeds) {
		t.Errorf("stale permissions were used: %d %s", status, out)
	}
	// Its keys:manage was taken away while the request waited.
	if _, err := f.st.SetAPIKeyScopes(context.Background(), k.ID, []string{"access:manage"}); err != nil {
		t.Fatal(err)
	}
	if status, out := call(keyAction("rotate", other.ID)); status != 403 || !strings.Contains(out, "insufficient_permissions") {
		t.Errorf("a key that lost keys:manage: %d %s", status, out)
	}
	// It was revoked while the request waited.
	if _, err := f.st.RevokeAPIKey(context.Background(), k.ID); err != nil {
		t.Fatal(err)
	}
	if status, out := call(`{"action":"create","name":"x","scopes":[]}`); status != 401 || !strings.Contains(out, "invalid_api_key") {
		t.Errorf("a revoked key: %d %s", status, out)
	}
}

// A person's role can change the same way.
func TestKeyChangeUsesTheRoleTheCallerHasNow(t *testing.T) {
	f := newKeyFixture(t)
	ctx := context.Background()
	const issuer = "https://lb.example.com/magic-link"
	cfg := &auth.BrowserAccess{Provider: auth.Provider{Kind: "direct"}, Origin: "https://lb.example.com", Permissions: []string{}}
	f.s.Browser = auth.NewBrowser(f.st, f.s.Root.Dir, cfg, f.s.Log)
	p := &auth.Policy{Roles: []auth.Role{{Name: "boss", Permissions: []string{"access:manage", "keys:manage"}}, {Name: "small", Permissions: []string{"keys:read"}}},
		Bindings: []auth.Binding{{Kind: "email", Role: "boss", Email: "boss@example.com"}, {Kind: "email", Role: "small", Email: "person@example.com"}}}
	if err := auth.ApplyPolicy(ctx, f.st, p); err != nil {
		t.Fatal(err)
	}
	// The session was resolved when it held keys:manage; the role no longer gives it.
	stale := &auth.Principal{Kind: "browser-session", ID: "browser:p", Permissions: []string{"keys:manage", "keys:read"},
		Identity: &auth.Identity{Issuer: issuer, Subject: "p", VerifiedEmail: "person@example.com"}}
	res := f.s.Call(ctx, stale, "POST", "/_localbase/api-keys", "application/json", []byte(`{"action":"create","name":"x","scopes":["keys:read"]}`))
	if res.Status != 403 || !strings.Contains(string(res.Body), "insufficient_permissions") {
		t.Errorf("a session whose role changed: %d %s", res.Status, res.Body)
	}
}

// Rotating another key takes it from whoever held it, so it can't be the last manager;
// the rotation of your own key puts the new secret in your hand, so it can.
func TestKeyRotateCannotLockEveryoneOut(t *testing.T) {
	f := newKeyFixture(t)
	only, onlySecret := f.key("only", 0, "keys:manage", "access:manage")
	host := &auth.Principal{Kind: "environment", Permissions: auth.Permissions}
	call := func(body string) (int, string) {
		res := f.s.Call(context.Background(), host, "POST", "/_localbase/api-keys", "application/json", []byte(body))
		return res.Status, string(res.Body)
	}
	if status, out := call(keyAction("rotate", only.ID)); status != 409 || !strings.Contains(out, "would_lock_out_admins") {
		t.Fatalf("rotating the last manager: %d %s", status, out)
	}
	if got := f.stored(only.ID); got.KeyHash != store.HashKey(onlySecret) {
		t.Fatal("the refused rotate changed the secret")
	}
	// Its own holder may, and gets the secret back.
	fresh := secretOf(t, f.expect(onlySecret, keyAction("rotate", only.ID), 200, ""))
	f.expect(fresh, `{"action":"create","name":"proof","scopes":[]}`, 201, "")
	// With another manager there is no lockout.
	f.key("second", 0, "access:manage")
	if status, out := call(keyAction("rotate", only.ID)); status != 200 {
		t.Errorf("rotating with another manager: %d %s", status, out)
	}
}

// A key may revoke or narrow itself: that is how a leaked key is killed with itself.
func TestKeyActingOnItself(t *testing.T) {
	f := newKeyFixture(t)
	// Only manager left: a revoke would leave nobody.
	last, lastSecret := f.key("last", 0, "keys:manage", "access:manage")
	f.expect(lastSecret, keyAction("revoke", last.ID), 409, "would_lock_out_admins")
	// Dropping access:manage from the key in use is refused, with its own words.
	f.key("other", 0, "access:manage")
	out := f.expect(lastSecret, keyAction("set-scopes", last.ID, "keys:manage"), 409, "would_lock_out_admins")
	if !strings.Contains(out, "remove access:manage from the API key you are using") {
		t.Errorf("message: %s", out)
	}
	// With another manager it can revoke itself; the response is the last it gets.
	out = f.expect(lastSecret, keyAction("revoke", last.ID), 200, "")
	if !strings.Contains(out, `"revokedAt"`) {
		t.Errorf("response: %s", out)
	}
	if got, _, _ := f.post(lastSecret, "/_localbase/api-keys", `{"action":"create","name":"x","scopes":[]}`); got != 401 {
		t.Errorf("a key that revoked itself: %d", got)
	}
	// A key that never held access:manage narrows itself freely, and isn't told it would lose it.
	narrow, narrowSecret := f.key("narrow", 0, "keys:manage", "keys:read", "models:read")
	f.expect(narrowSecret, keyAction("set-scopes", narrow.ID, "keys:manage", "keys:read"), 200, "")
	f.expect(narrowSecret, keyAction("revoke", narrow.ID), 200, "")
	// Rotating itself: the new secret works, the old one stops.
	self, selfSecret := f.key("self", 0, "keys:manage")
	fresh := secretOf(t, f.expect(selfSecret, keyAction("rotate", self.ID), 200, ""))
	if got, _, _ := f.post(selfSecret, "/_localbase/api-keys", `{"action":"create","name":"x","scopes":[]}`); got != 401 {
		t.Errorf("the old secret after rotating itself: %d", got)
	}
	f.expect(fresh, `{"action":"create","name":"proof","scopes":[]}`, 201, "")
}

// Two managers revoking each other at once must not leave nobody. Every request reaches
// the point just before its write together with the other, if nothing keeps them apart.
func TestManagersRevokingEachOtherLeaveOneStanding(t *testing.T) {
	f := newKeyFixture(t)
	a, aSecret := f.key("a", 0, "keys:manage", "access:manage")
	b, bSecret := f.key("b", 0, "keys:manage", "access:manage")
	const both = 2
	var arrived sync.WaitGroup
	arrived.Add(both)
	release := make(chan struct{})
	go func() { arrived.Wait(); close(release) }()
	f.s.beforeAdminWrite = func() {
		arrived.Done()
		select { // with the lock held the other can't arrive: go on after a moment
		case <-release:
		case <-time.After(200 * time.Millisecond):
			arrived.Add(1)
		}
	}
	var wg sync.WaitGroup
	statuses := make([]int, both)
	for i, call := range [][2]string{{aSecret, b.ID}, {bSecret, a.ID}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i], _, _ = f.post(call[0], "/_localbase/api-keys", keyAction("revoke", call[1]))
		}()
	}
	wg.Wait()
	alive := 0
	for _, id := range []string{a.ID, b.ID} {
		if auth.ActiveKey(*f.stored(id)) {
			alive++
		}
	}
	if alive != 1 {
		t.Fatalf("%d managers left after they revoked each other (statuses %v)", alive, statuses)
	}
}

// First-run and host paths mint keys with no caller to compare against, and the callers
// Connect and the keys page rely on hold everything they ask for. These must keep working.
func TestKeysStillWorkForFirstRunAndHostCallers(t *testing.T) {
	f := newKeyFixture(t)
	ctx := context.Background()
	// What `configure` and `local-base keys create` do: straight to the store, no caller.
	if _, _, err := f.st.CreateAPIKey(ctx, "default", auth.DefaultKeyScopes); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.st.CreateAPIKey(ctx, "full", auth.Permissions); err != nil {
		t.Fatal(err)
	}
	// The whole-machine callers hold every permission, so nothing they ask is refused:
	// --no-auth, LOCALBASE_API_KEY, and a first-run local session, whose principal has
	// no identity and every permission.
	callers := map[string]*auth.Principal{
		"environment key": {Kind: "environment", Permissions: auth.Permissions},
		"local session":   {Kind: "browser-session", ID: "local", Permissions: auth.Permissions},
		"no-auth page":    {Kind: "browser-session", ID: "no-auth", Permissions: auth.Permissions},
	}
	for name, p := range callers {
		// The Connect presets, and a key with everything.
		for _, scopes := range []string{`["inference:chat","models:read"]`, `["` + strings.Join(auth.DefaultKeyScopes, `","`) + `"]`, `["` + strings.Join(auth.Permissions, `","`) + `"]`} {
			res := f.s.Call(ctx, p, "POST", "/_localbase/api-keys", "application/json", []byte(`{"action":"create","name":"connect","scopes":`+scopes+`}`))
			if res.Status != 201 {
				t.Errorf("%s creating %s: %d %s", name, scopes, res.Status, res.Body)
			}
		}
	}
	// --no-auth, through the real route with no credential at all.
	f.s.NoAuth = true
	r := httptest.NewRequest(http.MethodPost, "/_localbase/api-keys", strings.NewReader(`{"action":"create","name":"open","scopes":["keys:manage"]}`))
	r.Host = "127.0.0.1"
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.s.ServeHTTP(w, r)
	if w.Code != 201 {
		t.Errorf("--no-auth: %d %s", w.Code, w.Body)
	}
	f.s.NoAuth = false
	// A person who holds keys:manage and chat can use the narrow Connect preset, and is
	// told in plain words why the one with every inference permission is refused.
	person := &auth.Principal{Kind: "browser-session", ID: "browser:x", Permissions: []string{"keys:manage", "inference:chat", "models:read"}}
	if res := f.s.Call(ctx, person, "POST", "/_localbase/api-keys", "application/json", []byte(`{"action":"create","name":"c","scopes":["inference:chat","models:read"]}`)); res.Status != 201 {
		t.Errorf("narrow preset: %d %s", res.Status, res.Body)
	}
	res := f.s.Call(ctx, person, "POST", "/_localbase/api-keys", "application/json", []byte(`{"action":"create","name":"c","scopes":["`+strings.Join(auth.DefaultKeyScopes, `","`)+`"]}`))
	if res.Status != 403 || !strings.Contains(string(res.Body), "You can only grant permissions you hold") {
		t.Errorf("wide preset: %d %s", res.Status, res.Body)
	}
}

// sessionPoster signs a person in with a real session whose role holds perms, and
// returns how to post as them through the /app/api forward and how to change the role.
func (f *keyFixture) sessionPoster(owner string, perms ...string) (post func(string) (int, string), setRole func(...string)) {
	t := f.t
	ctx := context.Background()
	const issuer = "https://idp.example.com"
	cfg := &auth.BrowserAccess{Origin: "https://lb.example.com", Permissions: []string{},
		Provider: auth.Provider{Kind: "direct", Registrations: []auth.Registration{{Kind: "oidc", ID: "idp", Name: "IdP", Issuer: issuer, ClientID: "cid",
			ClientAuthentication: &auth.ClientAuth{Kind: "none"}}}}}
	f.s.Browser = auth.NewBrowser(f.st, f.s.Root.Dir, cfg, f.s.Log)
	policy := func(perms ...string) *auth.Policy {
		return &auth.Policy{Roles: []auth.Role{{Name: "boss", Permissions: []string{"access:manage"}}, {Name: "holder", Permissions: perms}},
			Bindings: []auth.Binding{{Kind: "email", Role: "boss", Email: "boss@example.com"}, {Kind: "email", Role: "holder", Email: "person@example.com"}}}
	}
	if err := auth.ApplyPolicy(ctx, f.st, policy(perms...)); err != nil {
		t.Fatal(err)
	}
	token, err := auth.CreateSession(ctx, f.st, owner, auth.Identity{Issuer: issuer, Subject: "p", VerifiedEmail: "person@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	post = func(body string) (int, string) {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/app/api/_localbase/api-keys", strings.NewReader(body))
		r.Host = "lb.example.com"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "https://lb.example.com")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("X-Localbase-Ui", "1")
		r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: token})
		w := httptest.NewRecorder()
		f.s.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	return post, func(perms ...string) {
		t.Helper()
		if err := auth.ApplyPolicy(ctx, f.st, policy(perms...)); err != nil {
			t.Fatal(err)
		}
	}
}

// The same rules through a real session cookie and the /app/api forward: what a person
// holds is what their role gives them on this request, and the audit names their id.
func TestKeyRulesThroughARealSession(t *testing.T) {
	f := newKeyFixture(t)
	post, setRole := f.sessionPoster("browser:person-hash", "keys:manage", "keys:read", "models:read")
	wide, _ := f.key("wide", 0, "keys:manage", "access:manage")
	if status, out := post(`{"action":"create","name":"more","scopes":["access:manage"]}`); status != 403 || !strings.Contains(out, exceeds) {
		t.Fatalf("a wider key through the session: %d %s", status, out)
	}
	if status, out := post(keyAction("rotate", wide.ID)); status != 403 || !strings.Contains(out, exceeds) {
		t.Fatalf("rotating a wider key through the session: %d %s", status, out)
	}
	if status, out := post(`{"action":"create","name":"mine","scopes":["keys:read"]}`); status != 201 {
		t.Fatalf("a key within the session's role: %d %s", status, out)
	}
	// Narrowing the role takes effect on the session's next request.
	setRole("keys:read")
	if status, out := post(`{"action":"create","name":"late","scopes":["keys:read"]}`); status != 403 || !strings.Contains(out, "insufficient_permissions") {
		t.Fatalf("after the role was narrowed: %d %s", status, out)
	}
}
