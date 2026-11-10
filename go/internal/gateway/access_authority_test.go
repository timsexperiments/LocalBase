package gateway

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
)

const (
	wideRole   = `{"name":"owner","description":"","permissions":["inference:chat","models:manage","keys:manage","keys:read","access:manage","access:read","system:manage"]}`
	allRoles   = wideRole + "," + gatewaytest.ChatRole + "," + gatewaytest.AdminRole
	ownerBound = `{"kind":"email","role":"owner","email":"owner@example.test"}`

	oidc       = `{"kind":"oidc","id":"other","name":"Other","issuer":"https://idp.example.test","clientId":"x","clientAuthentication":{"kind":"none"}}`
	mailConfig = `{"host":"smtp.example.test","port":587,"security":"starttls","authentication":{"kind":"none"},"from":"x@example.test"}`
)

// accessFixture has a provider, a policy, and managed people, set up by a key holding
// everything. Its delegate holds access:manage and whatever else a test gives it.
type accessFixture struct {
	*keyFixture
	owner string
}

func newAccessFixture(t *testing.T) *accessFixture {
	t.Helper()
	f := &accessFixture{keyFixture: newKeyFixture(t)}
	_, f.owner = f.key("owner", 0, auth.Permissions...)
	f.access(f.owner, gatewaytest.ConfigureCloudflare, 200, "")
	f.access(f.owner, gatewaytest.PolicyRequest("null", allRoles, ownerBound, "null"), 200, "")
	f.access(f.owner, `{"action":"invite-user","email":"wide@example.test","roles":["owner"]}`, 201, "")
	f.access(f.owner, `{"action":"invite-user","email":"small@example.test","roles":["chat"]}`, 201, "")
	return f
}

func (f *accessFixture) access(secret, body string, status int, code string) string {
	f.t.Helper()
	got, gotCode, out := f.post(secret, gatewaytest.AccessRoute, body)
	if got != status || gotCode != code {
		f.t.Fatalf("%s: %d %s, want %d %q", body, got, out, status, code)
	}
	return out
}

// state is the policy and the people, to show a refused change touched nothing.
func (f *accessFixture) state() string {
	f.t.Helper()
	res := f.s.Call(context.Background(), &auth.Principal{Kind: "environment", Permissions: auth.Permissions}, "GET", gatewaytest.AccessRoute, "", nil)
	var v struct {
		Config   json.RawMessage `json:"config"`
		Revision *string         `json:"policyRevision"`
		Users    json.RawMessage `json:"users"`
		Email    json.RawMessage `json:"emailDelivery"`
	}
	if err := json.Unmarshal(res.Body, &v); err != nil {
		f.t.Fatal(err)
	}
	rev := "none"
	if v.Revision != nil {
		rev = *v.Revision
	}
	return rev + string(v.Config) + string(v.Users) + string(v.Email)
}

func (f *accessFixture) revision() string {
	f.t.Helper()
	res := f.s.Call(context.Background(), &auth.Principal{Kind: "environment", Permissions: auth.Permissions}, "GET", gatewaytest.AccessRoute, "", nil)
	var v struct {
		Revision *string `json:"policyRevision"`
	}
	json.Unmarshal(res.Body, &v)
	if v.Revision == nil {
		return "null"
	}
	return `"` + *v.Revision + `"`
}

// A holder of access:manage can't reach past itself, and can't take from anyone wider.
func TestAccessChangesStayWithinTheCaller(t *testing.T) {
	f := newAccessFixture(t)
	_, delegate := f.key("delegate", 0, "access:manage", "access:read")
	before := f.state()
	rev := f.revision()
	narrowOwner := strings.Replace(allRoles, wideRole, `{"name":"owner","description":"","permissions":["inference:chat"]}`, 1)
	for name, body := range map[string]string{
		"bind a domain to a wider role":   gatewaytest.PolicyRequest(rev, allRoles, ownerBound+`,{"kind":"email-domain","role":"owner","domain":"stranger.test"}`, "null"),
		"bind an email to a wider role":   gatewaytest.PolicyRequest(rev, allRoles, ownerBound+`,{"kind":"email","role":"owner","email":"mine@example.test"}`, "null"),
		"bind a subject to a wider role":  gatewaytest.PolicyRequest(rev, allRoles, ownerBound+`,{"kind":"subject","role":"owner","issuer":"https://idp.example.test","subject":"me"}`, "null"),
		"make a wider role the default":   gatewaytest.PolicyRequest(rev, allRoles, ownerBound, `"owner"`),
		"add to a role past itself":       gatewaytest.PolicyRequest(rev, strings.Replace(allRoles, gatewaytest.ChatRole, `{"name":"chat","description":"","permissions":["inference:chat","keys:manage"]}`, 1), ownerBound, "null"),
		"create a role wider than itself": gatewaytest.PolicyRequest(rev, allRoles+`,{"name":"mine","description":"","permissions":["keys:manage"]}`, ownerBound, "null"),
		"remove a wider binding":          gatewaytest.PolicyRequest(rev, allRoles, `{"kind":"email","role":"admin","email":"mine@example.test"}`, "null"),
		"narrow a wider role":             gatewaytest.PolicyRequest(rev, narrowOwner, ownerBound+`,{"kind":"email","role":"admin","email":"mine@example.test"}`, "null"),
		"move a binding off a wider role": gatewaytest.PolicyRequest(rev, allRoles, `{"kind":"email","role":"admin","email":"owner@example.test"}`, "null"),
		"clear the policy":                `{"action":"clear-policy","expectedPolicyRevision":` + rev + `}`,
		"invite with a wider role":        `{"action":"invite-user","email":"mine@example.test","roles":["owner"]}`,
		"give a person a wider role":      `{"action":"replace-user-roles","email":"small@example.test","roles":["owner"]}`,
		"re-role a wider person":          `{"action":"replace-user-roles","email":"wide@example.test","roles":["admin"]}`,
		"disable a wider person":          `{"action":"disable-user","email":"wide@example.test"}`,
		"enable a wider person":           `{"action":"enable-user","email":"wide@example.test"}`,
		"remove a wider person":           `{"action":"remove-user","email":"wide@example.test"}`,
		"register a provider":             `{"action":"upsert-oidc","registration":` + oidc + `,"origin":"https://lb.example.test","permissions":[]}`,
		"replace the provider":            gatewaytest.ConfigureCloudflare,
		"remove a registration":           `{"action":"remove-registration","registrationId":"google"}`,
		"turn off sign-in":                `{"action":"disable"}`,
		"point email delivery elsewhere":  `{"action":"configure-email-delivery","config":` + mailConfig + `}`,
		"turn off email delivery":         `{"action":"disable-email-delivery"}`,
	} {
		t.Run(name, func(t *testing.T) {
			got, code, out := f.post(delegate, gatewaytest.AccessRoute, body)
			if got != 403 || code != exceeds {
				t.Errorf("%d %s", got, out)
			}
			if f.state() != before {
				t.Error("a refused change changed something")
			}
		})
	}
	if _, err := os.Stat(f.s.cfg.Root.Dir + "/email-delivery.json"); err == nil {
		t.Error("a refused change wrote email delivery")
	}
}

// Whatever it holds, it can manage. A wider role it leaves alone stays as it is.
func TestAccessChangesWithinTheCallerStillWork(t *testing.T) {
	f := newAccessFixture(t)
	_, delegate := f.key("delegate", 0, "access:manage", "access:read", "inference:chat")
	withBinding := ownerBound + `,{"kind":"email","role":"chat","email":"mine@example.test"}`
	f.access(delegate, gatewaytest.PolicyRequest(f.revision(), allRoles, withBinding, "null"), 200, "")
	f.access(delegate, gatewaytest.PolicyRequest(f.revision(), allRoles+`,{"name":"mine","description":"","permissions":["inference:chat"]}`, withBinding, "null"), 200, "")
	f.access(delegate, gatewaytest.PolicyRequest(f.revision(), allRoles+`,{"name":"mine","description":"changed words","permissions":["inference:chat"]}`, withBinding, `"chat"`), 200, "")
	f.access(delegate, gatewaytest.PolicyRequest(f.revision(), allRoles, withBinding, "null"), 200, "")
	f.access(delegate, `{"action":"invite-user","email":"new@example.test","roles":["chat"]}`, 201, "")
	f.access(delegate, `{"action":"replace-user-roles","email":"small@example.test","roles":["chat","admin"]}`, 200, "")
	f.access(delegate, `{"action":"disable-user","email":"small@example.test"}`, 200, "")
	f.access(delegate, `{"action":"enable-user","email":"small@example.test"}`, 200, "")
	f.access(delegate, `{"action":"remove-user","email":"small@example.test"}`, 200, "")
	// An unknown person or role keeps its own answer.
	f.access(delegate, `{"action":"disable-user","email":"nobody@example.test"}`, 404, "managed_user_not_found")
	f.access(delegate, `{"action":"invite-user","email":"x@example.test","roles":["missing"]}`, 400, "role_not_found")
	// Someone holding everything a sign-in can reach may change sign-in itself.
	f.access(f.owner, `{"action":"upsert-oidc","registration":`+oidc+`,"origin":"https://lb.example.test","permissions":["inference:chat"]}`, 200, "")
	f.access(f.owner, `{"action":"configure-email-delivery","config":`+mailConfig+`}`, 200, "")
	f.access(f.owner, `{"action":"disable-email-delivery"}`, 200, "")
	f.access(f.owner, `{"action":"disable"}`, 200, "")
}

// Giving someone a role needs that role, even when the person is within the caller.
func TestReRoleNeedsTheNewRole(t *testing.T) {
	f := newAccessFixture(t)
	_, delegate := f.key("delegate", 0, "access:manage", "access:read", "inference:chat")
	f.access(delegate, `{"action":"replace-user-roles","email":"small@example.test","roles":["owner"]}`, 403, exceeds)
	f.access(delegate, `{"action":"replace-user-roles","email":"small@example.test","roles":["admin","chat"]}`, 200, "")
}

// Someone already removed gets the usual answer, whatever a binding on their address says.
func TestRemovedPersonIsNotFound(t *testing.T) {
	f := newAccessFixture(t)
	_, delegate := f.key("delegate", 0, "access:manage", "access:read", "inference:chat")
	f.access(f.owner, gatewaytest.PolicyRequest(f.revision(), allRoles, ownerBound+`,{"kind":"email","role":"owner","email":"gone@example.test"}`, "null"), 200, "")
	f.access(f.owner, `{"action":"invite-user","email":"gone@example.test","roles":["chat"]}`, 201, "")
	f.access(f.owner, `{"action":"remove-user","email":"gone@example.test"}`, 200, "")
	f.access(delegate, `{"action":"disable-user","email":"gone@example.test"}`, 404, "managed_user_not_found")
}

// Editing a role needs what it holds before and what it holds after, each on its own.
func TestRoleEditNeedsBothSides(t *testing.T) {
	f := newAccessFixture(t)
	_, delegate := f.key("delegate", 0, "access:manage", "access:read", "inference:chat")
	withAdmin := ownerBound + `,{"kind":"email","role":"admin","email":"mine@example.test"}`
	f.access(f.owner, gatewaytest.PolicyRequest(f.revision(), allRoles, withAdmin, "null"), 200, "")
	before := f.state()
	// Before is within the caller, after isn't.
	f.access(delegate, gatewaytest.PolicyRequest(f.revision(), strings.Replace(allRoles, gatewaytest.ChatRole, `{"name":"chat","description":"","permissions":["inference:chat","keys:manage"]}`, 1), withAdmin, "null"), 403, exceeds)
	// After is within the caller, before isn't.
	f.access(delegate, gatewaytest.PolicyRequest(f.revision(), strings.Replace(allRoles, wideRole, `{"name":"owner","description":"","permissions":["inference:chat"]}`, 1), withAdmin, "null"), 403, exceeds)
	if f.state() != before {
		t.Error("a refused edit changed something")
	}
	// Both within it, and it goes through.
	f.access(delegate, gatewaytest.PolicyRequest(f.revision(), strings.Replace(allRoles, gatewaytest.ChatRole, `{"name":"chat","description":"","permissions":["access:read"]}`, 1), withAdmin, "null"), 200, "")
}

// What a sign-in can grant includes the provider-wide permissions, even under a policy:
// clearing the policy would switch them on.
func TestSignInChangeNeedsTheProviderWidePermissions(t *testing.T) {
	f := &accessFixture{keyFixture: newKeyFixture(t)}
	_, f.owner = f.key("owner", 0, auth.Permissions...)
	f.access(f.owner, strings.Replace(gatewaytest.ConfigureCloudflare, `"permissions":["inference:chat"]`, `"permissions":["models:manage"]`, 1), 200, "")
	f.access(f.owner, gatewaytest.PolicyRequest("null", gatewaytest.ChatRole+","+gatewaytest.AdminRole, `{"kind":"email","role":"admin","email":"mine@example.test"}`, "null"), 200, "")
	// It holds every role in the policy and every permission it asks to set.
	_, delegate := f.key("delegate", 0, "access:manage", "access:read", "inference:chat")
	f.access(delegate, gatewaytest.ConfigureCloudflare, 403, exceeds)
	_, wider := f.key("wider", 0, "access:manage", "access:read", "inference:chat", "models:manage")
	f.access(wider, gatewaytest.ConfigureCloudflare, 200, "")
}

// A person's permissions are all they resolve to, not only the roles on their record.
func TestAccessCountsEverythingAPersonResolvesTo(t *testing.T) {
	f := newAccessFixture(t)
	_, delegate := f.key("delegate", 0, "access:manage", "access:read", "inference:chat")
	for name, bindings := range map[string]string{
		"email":  ownerBound + `,{"kind":"email","role":"owner","email":"small@example.test"}`,
		"domain": ownerBound + `,{"kind":"email-domain","role":"owner","domain":"example.test"}`,
	} {
		t.Run(name, func(t *testing.T) {
			f.access(f.owner, gatewaytest.PolicyRequest(f.revision(), allRoles, bindings, "null"), 200, "")
			f.access(delegate, `{"action":"disable-user","email":"small@example.test"}`, 403, exceeds)
			f.access(f.owner, gatewaytest.PolicyRequest(f.revision(), allRoles, ownerBound, "null"), 200, "")
		})
	}
	t.Run("default role", func(t *testing.T) {
		f.access(f.owner, gatewaytest.PolicyRequest(f.revision(), allRoles, ownerBound, `"owner"`), 200, "")
		f.access(delegate, `{"action":"disable-user","email":"small@example.test"}`, 403, exceeds)
		// Changing a wider default is refused, too.
		f.access(delegate, gatewaytest.PolicyRequest(f.revision(), allRoles, ownerBound, `"chat"`), 403, exceeds)
		f.access(f.owner, gatewaytest.PolicyRequest(f.revision(), allRoles, ownerBound, "null"), 200, "")
	})
	// With no such reach, the same action works.
	f.access(delegate, `{"action":"disable-user","email":"small@example.test"}`, 200, "")
}

// Without a policy, everyone signing in holds the provider-wide permissions. A first
// policy replaces them, and clearing a policy brings them back for everyone.
func TestPolicyAndProviderWidePermissions(t *testing.T) {
	f := &accessFixture{keyFixture: newKeyFixture(t)}
	_, f.owner = f.key("owner", 0, auth.Permissions...)
	f.access(f.owner, strings.Replace(gatewaytest.ConfigureCloudflare, `"permissions":["inference:chat"]`, `"permissions":["inference:chat","models:manage"]`, 1), 200, "")
	_, delegate := f.key("delegate", 0, "access:manage", "access:read", "inference:chat")
	first := gatewaytest.PolicyRequest("null", gatewaytest.ChatRole+","+gatewaytest.AdminRole, `{"kind":"email","role":"admin","email":"mine@example.test"}`, "null")
	f.access(delegate, first, 403, exceeds)
	f.access(f.owner, first, 200, "")
	// It holds both roles' permissions, but not the provider-wide models:manage.
	_, narrow := f.key("narrow", 0, "access:manage", "access:read", "inference:chat")
	f.access(narrow, `{"action":"clear-policy","expectedPolicyRevision":`+f.revision()+`}`, 403, exceeds)
	// Setting provider-wide permissions past the caller is a grant to everyone.
	_, signIn := f.key("sign-in", 0, "access:manage", "access:read", "inference:chat", "models:manage")
	f.access(signIn, strings.Replace(gatewaytest.ConfigureCloudflare, `"permissions":["inference:chat"]`, `"permissions":["system:manage"]`, 1), 403, exceeds)
	f.access(signIn, strings.Replace(gatewaytest.ConfigureCloudflare, `"permissions":["inference:chat"]`, `"permissions":["models:manage"]`, 1), 200, "")
}

// A session is held to what its role gives it on this request.
func TestAccessRulesThroughARealSession(t *testing.T) {
	f := newKeyFixture(t, withSessionAccess(t))
	post, setRole := f.sessionPoster("browser:person-hash", "access:manage", "access:read")
	postAccess := func(body string) (int, string) {
		t.Helper()
		return post(body, gatewaytest.AccessRoute)
	}
	if status, out := postAccess(`{"action":"invite-user","email":"x@example.com","roles":["boss"]}`); status != 201 {
		t.Fatalf("a role within its own: %d %s", status, out)
	}
	// Grow the boss role past the session, which is held to its role alone.
	if err := auth.ApplyPolicy(context.Background(), f.st, &auth.Policy{
		Roles:    []auth.Role{{Name: "boss", Permissions: []string{"access:manage", "keys:manage"}}, {Name: "holder", Permissions: []string{"access:manage", "access:read"}}},
		Bindings: []auth.Binding{{Kind: "email", Role: "boss", Email: "boss@example.com", Issuer: "https://idp.example.com"}, {Kind: "email", Role: "holder", Email: "person@example.com", Issuer: "https://idp.example.com"}},
	}); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"action":"invite-user","email":"y@example.com","roles":["boss"]}`,
		`{"action":"disable-user","email":"x@example.com"}`,
		`{"action":"configure-email-delivery","config":` + mailConfig + `}`,
		`{"action":"disable"}`,
	} {
		if status, out := postAccess(body); status != 403 || !strings.Contains(out, exceeds) {
			t.Errorf("%s: %d %s", body, status, out)
		}
	}
	setRole("access:read")
	if status, out := postAccess(`{"action":"disable"}`); status != 403 || !strings.Contains(out, "insufficient_permissions") {
		t.Errorf("after the role was narrowed: %d %s", status, out)
	}
}

// What was true when a request was authenticated may not be true when its turn comes.
func TestAccessChangeUsesWhatTheCallerHoldsNow(t *testing.T) {
	f := newAccessFixture(t)
	k, _ := f.key("caller", 0, "access:manage", "access:read")
	stale := &auth.Principal{Kind: "api-key", ID: k.ID, Permissions: auth.Permissions}
	call := func(body string) (int, string) {
		res := f.s.Call(context.Background(), stale, "POST", gatewaytest.AccessRoute, "application/json", []byte(body))
		return res.Status, string(res.Body)
	}
	if status, out := call(`{"action":"disable"}`); status != 403 || !strings.Contains(out, exceeds) {
		t.Errorf("stale permissions were used: %d %s", status, out)
	}
	if _, err := f.st.SetAPIKeyScopes(context.Background(), k.ID, []string{"access:read"}); err != nil {
		t.Fatal(err)
	}
	if status, out := call(`{"action":"disable"}`); status != 403 || !strings.Contains(out, "insufficient_permissions") {
		t.Errorf("a key that lost access:manage: %d %s", status, out)
	}
	if _, err := f.st.RevokeAPIKey(context.Background(), k.ID); err != nil {
		t.Fatal(err)
	}
	if status, out := call(`{"action":"disable"}`); status != 401 || !strings.Contains(out, "invalid_api_key") {
		t.Errorf("a revoked key: %d %s", status, out)
	}
}

// The access page and API say how many rows name no provider, so someone can act on it.
func TestAccessReportsWhatIsNotBound(t *testing.T) {
	f := newAccessFixture(t)
	ctx := context.Background()
	host := &auth.Principal{Kind: "environment", Permissions: auth.Permissions}
	count := func() any {
		t.Helper()
		var v map[string]any
		json.Unmarshal(f.s.Call(ctx, host, "GET", gatewaytest.AccessRoute, "", nil).Body, &v)
		return v["unboundBindings"]
	}
	if got := count(); got != nil {
		t.Errorf("everything named: %v", got)
	}
	if _, err := f.st.DB.Exec(`DELETE FROM go_binding_issuers`); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != float64(1) {
		t.Errorf("one binding named by nothing: %v", got)
	}
	// With no provider there is nothing to choose, so nothing to report.
	if _, err := auth.DisableAccess(f.s.cfg.Root.Dir); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != nil {
		t.Errorf("no provider: %v", got)
	}
}

// A refused access change is on the record, with who asked and why.
func TestAccessRefusalsAreAudited(t *testing.T) {
	f := newAccessFixture(t)
	k, delegate := f.key("delegate", 0, "access:manage", "access:read")
	f.access(delegate, `{"action":"invite-user","email":"secret.person@example.test","roles":["owner"]}`, 403, exceeds)
	want := `"event":"audit.access-refused","action":"invite-user","actor_kind":"api-key","actor_id":"` + k.ID + `"`
	if !strings.Contains(f.logs.String(), want) || !strings.Contains(f.logs.String(), `"reason":"`+exceeds+`"`) {
		t.Errorf("no refusal on record:\n%s", f.logs)
	}
	if strings.Contains(f.logs.String(), "secret.person") {
		t.Errorf("the log holds an email:\n%s", f.logs)
	}
}

// Through the API, an address gets the provider that vouches for it: the only one there
// is, or the one named. With several, nobody guesses.
func TestAddressesNameTheirProvider(t *testing.T) {
	f := &accessFixture{keyFixture: newKeyFixture(t)}
	_, f.owner = f.key("owner", 0, auth.Permissions...)
	const (
		a        = "https://a.example.test"
		b        = "https://b.example.test"
		unnamed  = `{"kind":"email","role":"admin","email":"me@example.test"}`
		onDomain = `{"kind":"email-domain","role":"chat","domain":"example.test"}`
	)
	oidcAt := func(id, issuer string) string {
		return `{"action":"upsert-oidc","registration":{"kind":"oidc","id":"` + id + `","name":"P","issuer":"` + issuer + `","clientId":"x","clientAuthentication":{"kind":"none"}},"origin":"https://lb.example.test","permissions":["inference:chat"]}`
	}
	roles := gatewaytest.ChatRole + "," + gatewaytest.AdminRole
	// No provider: the answer stays that one is needed first, whatever is named.
	f.access(f.owner, gatewaytest.PolicyRequest("null", roles, unnamed, "null"), 409, "provider_not_configured")
	named := `{"kind":"email","role":"admin","email":"me@example.test","issuer":"` + a + `"}`
	f.access(f.owner, gatewaytest.PolicyRequest("null", roles, named, "null"), 409, "provider_not_configured")
	f.access(f.owner, `{"action":"invite-user","email":"x@example.test","roles":["chat"],"issuer":"`+a+`"}`, 409, "provider_not_configured")

	// One provider: nothing to guess.
	f.access(f.owner, oidcAt("a", a), 200, "")
	out := f.access(f.owner, gatewaytest.PolicyRequest("null", roles, unnamed+","+onDomain, "null"), 200, "")
	if strings.Count(out, `"issuer":"`+a+`"`) != 2 {
		t.Errorf("one provider: %s", out)
	}
	out = f.access(f.owner, `{"action":"invite-user","email":"first@example.test","roles":["chat"]}`, 201, "")
	if !strings.Contains(out, `"issuer":"`+a+`"`) {
		t.Errorf("an invitation with one provider: %s", out)
	}
	// A provider that isn't configured can't be named on anything new.
	f.access(f.owner, gatewaytest.PolicyRequest(f.revision(), roles, unnamed+`,{"kind":"email","role":"chat","email":"y@example.test","issuer":"`+b+`"}`, "null"), 400, "issuer_unknown")
	f.access(f.owner, `{"action":"invite-user","email":"x@example.test","roles":["chat"],"issuer":"`+b+`"}`, 400, "issuer_unknown")

	// Two providers: named or refused, in words that list them.
	f.access(f.owner, oidcAt("b", b), 200, "")
	out = f.access(f.owner, gatewaytest.PolicyRequest(f.revision(), roles, named+`,{"kind":"email","role":"chat","email":"new@example.test"}`, "null"), 400, "issuer_required")
	if !strings.Contains(out, a) || !strings.Contains(out, b) || !strings.Contains(out, "new@example.test") {
		t.Errorf("message: %s", out)
	}
	out = f.access(f.owner, `{"action":"invite-user","email":"x@example.test","roles":["chat"]}`, 400, "issuer_required")
	if !strings.Contains(out, a) || !strings.Contains(out, b) {
		t.Errorf("message: %s", out)
	}
	f.access(f.owner, `{"action":"invite-user","email":"x@example.test","roles":["chat"],"issuer":"`+b+`"}`, 201, "")
	f.access(f.owner, gatewaytest.PolicyRequest(f.revision(), roles, named+`,{"kind":"email-domain","role":"chat","domain":"example.test","issuer":"`+a+`"},{"kind":"email","role":"chat","email":"both@example.test","issuer":"`+a+`"},{"kind":"email","role":"chat","email":"both@example.test","issuer":"`+b+`"}`, "null"), 200, "")
	// A person adding their own address gets the provider they signed in with.
	me := &auth.Principal{Kind: "browser-session", ID: "browser:me", Permissions: auth.Permissions, Identity: &auth.Identity{Issuer: b, Subject: "me", VerifiedEmail: "starter@example.test"}}
	body := gatewaytest.PolicyRequest(f.revision(), roles, named+`,{"kind":"email","role":"admin","email":"starter@example.test"}`, "null")
	res := f.s.Call(context.Background(), me, "POST", gatewaytest.AccessRoute, "application/json", []byte(body))
	if res.Status != 200 || !strings.Contains(string(res.Body), `"email":"starter@example.test","issuer":"`+b+`"`) {
		t.Errorf("your own address: %d %s", res.Status, res.Body)
	}
	// A binding whose provider has gone may stay, so an unrelated edit still works.
	f.access(f.owner, `{"action":"remove-registration","registrationId":"b"}`, 200, "")
	res = f.s.Call(context.Background(), me, "GET", gatewaytest.AccessRoute, "", nil)
	var state struct {
		Policy   json.RawMessage `json:"policy"`
		Revision string          `json:"policyRevision"`
	}
	if err := json.Unmarshal(res.Body, &state); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(state.Policy), b) {
		t.Fatalf("policy: %s", state.Policy)
	}
	f.access(f.owner, `{"action":"apply-policy","expectedPolicyRevision":"`+state.Revision+`","policy":`+string(state.Policy)+`}`, 200, "")
}

// Who can sign in is part of who can manage access: a change to providers can't leave
// nobody, or take the caller's own access from under it.
func TestSignInChangesCannotLockEveryoneOut(t *testing.T) {
	f := &accessFixture{keyFixture: newKeyFixture(t)}
	ctx := context.Background()
	host := &auth.Principal{Kind: "environment", Permissions: auth.Permissions}
	call := func(p *auth.Principal, body string) (int, string) {
		res := f.s.Call(ctx, p, "POST", gatewaytest.AccessRoute, "application/json", []byte(body))
		return res.Status, string(res.Body)
	}
	expect := func(p *auth.Principal, body string, status int, code string) {
		t.Helper()
		if got, out := call(p, body); got != status || !strings.Contains(out, code) {
			t.Fatalf("%s: %d %s, want %d %s", body, got, out, status, code)
		}
	}
	const (
		a = "https://a.example.test"
		b = "https://b.example.test"
	)
	reg := func(id, issuer string) string {
		return `{"action":"upsert-oidc","registration":{"kind":"oidc","id":"` + id + `","name":"P","issuer":"` + issuer + `","clientId":"x","clientAuthentication":{"kind":"none"}},"origin":"https://lb.example.test","permissions":["inference:chat"]}`
	}
	const lockedOut = "would_lock_out_admins"
	expect(host, reg("a", a), 200, "")
	expect(host, reg("b", b), 200, "")
	expect(host, gatewaytest.PolicyRequest("null", gatewaytest.ChatRole+","+gatewaytest.AdminRole, `{"kind":"email","role":"admin","email":"me@example.test","issuer":"`+a+`"}`, "null"), 200, "")
	// The only manager signs in through a. Everything that takes a away is refused.
	expect(host, `{"action":"remove-registration","registrationId":"a"}`, 409, lockedOut)
	expect(host, reg("a", "https://moved.example.test"), 409, lockedOut)
	expect(host, gatewaytest.ConfigureCloudflare, 409, lockedOut)
	expect(host, `{"action":"disable"}`, 409, lockedOut)
	if cfg, err := auth.LoadAccess(f.s.cfg.Root.Dir); err != nil || len(cfg.Issuers()) != 2 {
		t.Fatalf("a refused change was written: %v %v", cfg.Issuers(), err)
	}
	// b isn't what the manager depends on.
	expect(host, `{"action":"remove-registration","registrationId":"b"}`, 200, "")
	// Removing the last provider, with no one else to manage, is refused the same way.
	expect(host, `{"action":"remove-registration","registrationId":"a"}`, 409, lockedOut)
	// A manager that needs no sign-in makes the same changes possible.
	f.key("spare", 0, "access:manage")
	expect(host, reg("b", b), 200, "")
	expect(host, `{"action":"remove-registration","registrationId":"a"}`, 200, "")
	expect(host, `{"action":"disable"}`, 200, "")
}

// A caller signed in through a provider can't remove the provider from under itself.
func TestSignInChangeCannotLockOutTheCaller(t *testing.T) {
	f := &accessFixture{keyFixture: newKeyFixture(t)}
	ctx := context.Background()
	const a, b = "https://a.example.test", "https://b.example.test"
	host := &auth.Principal{Kind: "environment", Permissions: auth.Permissions}
	me := &auth.Principal{Kind: "browser-session", ID: "browser:me", Permissions: auth.Permissions, Identity: &auth.Identity{Issuer: a, Subject: "me", VerifiedEmail: "me@example.test"}}
	call := func(p *auth.Principal, body string) (int, string) {
		res := f.s.Call(ctx, p, "POST", gatewaytest.AccessRoute, "application/json", []byte(body))
		return res.Status, string(res.Body)
	}
	reg := func(id, issuer string) string {
		return `{"action":"upsert-oidc","registration":{"kind":"oidc","id":"` + id + `","name":"P","issuer":"` + issuer + `","clientId":"x","clientAuthentication":{"kind":"none"}},"origin":"https://lb.example.test","permissions":["inference:chat"]}`
	}
	for _, body := range []string{reg("a", a), reg("b", b),
		gatewaytest.PolicyRequest("null", gatewaytest.ChatRole+","+gatewaytest.AdminRole, `{"kind":"email","role":"admin","email":"me@example.test","issuer":"`+a+`"},{"kind":"email","role":"admin","email":"other@example.test","issuer":"`+b+`"}`, "null")} {
		if status, out := call(host, body); status != 200 {
			t.Fatalf("%d %s", status, out)
		}
	}
	// Someone else can still manage through b, so only the caller's own access is at stake.
	if status, out := call(me, `{"action":"remove-registration","registrationId":"a"}`); status != 409 || !strings.Contains(out, "would_lock_out_admins") || !strings.Contains(out, "your own access management") {
		t.Errorf("%d %s", status, out)
	}
	if status, out := call(host, `{"action":"remove-registration","registrationId":"a"}`); status != 200 {
		t.Errorf("someone else may: %d %s", status, out)
	}
}
