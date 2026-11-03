package gateway

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

const accessRoute = "/_localbase/access-management"

const (
	wideRole   = `{"name":"owner","description":"","permissions":["inference:chat","models:manage","keys:manage","keys:read","access:manage","access:read","system:manage"]}`
	chatRole   = `{"name":"chat","description":"","permissions":["inference:chat"]}`
	adminRole  = `{"name":"admin","description":"","permissions":["access:read","access:manage"]}`
	allRoles   = wideRole + "," + chatRole + "," + adminRole
	ownerBound = `{"kind":"email","role":"owner","email":"owner@example.test"}`

	cloudflare = `{"action":"configure-cloudflare","provider":{"kind":"cloudflare-access","teamDomain":"team.cloudflareaccess.com","audience":"aud"},"origin":"https://lb.example.test","permissions":["inference:chat"]}`
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
	f.access(f.owner, cloudflare, 200, "")
	f.access(f.owner, policyRequest("null", allRoles, ownerBound, "null"), 200, "")
	f.access(f.owner, `{"action":"invite-user","email":"wide@example.test","roles":["owner"]}`, 201, "")
	f.access(f.owner, `{"action":"invite-user","email":"small@example.test","roles":["chat"]}`, 201, "")
	return f
}

func (f *accessFixture) access(secret, body string, status int, code string) string {
	f.t.Helper()
	got, gotCode, out := f.post(secret, accessRoute, body)
	if got != status || gotCode != code {
		f.t.Fatalf("%s: %d %s, want %d %q", body, got, out, status, code)
	}
	return out
}

// state is the policy and the people, to show a refused change touched nothing.
func (f *accessFixture) state() string {
	f.t.Helper()
	res := f.s.Call(context.Background(), &auth.Principal{Kind: "environment", Permissions: auth.Permissions}, "GET", accessRoute, "", nil)
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
	res := f.s.Call(context.Background(), &auth.Principal{Kind: "environment", Permissions: auth.Permissions}, "GET", accessRoute, "", nil)
	var v struct {
		Revision *string `json:"policyRevision"`
	}
	json.Unmarshal(res.Body, &v)
	if v.Revision == nil {
		return "null"
	}
	return `"` + *v.Revision + `"`
}

func policyRequest(revision, roles, bindings, defaultRole string) string {
	return `{"action":"apply-policy","expectedPolicyRevision":` + revision + `,"policy":{"roles":[` + roles + `],"bindings":[` + bindings + `],"defaultRole":` + defaultRole + `}}`
}

// A holder of access:manage can't reach past itself, and can't take from anyone wider.
func TestAccessChangesStayWithinTheCaller(t *testing.T) {
	f := newAccessFixture(t)
	_, delegate := f.key("delegate", 0, "access:manage", "access:read")
	before := f.state()
	rev := f.revision()
	narrowOwner := strings.Replace(allRoles, wideRole, `{"name":"owner","description":"","permissions":["inference:chat"]}`, 1)
	for name, body := range map[string]string{
		"bind a domain to a wider role":   policyRequest(rev, allRoles, ownerBound+`,{"kind":"email-domain","role":"owner","domain":"stranger.test"}`, "null"),
		"bind an email to a wider role":   policyRequest(rev, allRoles, ownerBound+`,{"kind":"email","role":"owner","email":"mine@example.test"}`, "null"),
		"bind a subject to a wider role":  policyRequest(rev, allRoles, ownerBound+`,{"kind":"subject","role":"owner","issuer":"https://idp.example.test","subject":"me"}`, "null"),
		"make a wider role the default":   policyRequest(rev, allRoles, ownerBound, `"owner"`),
		"add to a role past itself":       policyRequest(rev, strings.Replace(allRoles, chatRole, `{"name":"chat","description":"","permissions":["inference:chat","keys:manage"]}`, 1), ownerBound, "null"),
		"create a role wider than itself": policyRequest(rev, allRoles+`,{"name":"mine","description":"","permissions":["keys:manage"]}`, ownerBound, "null"),
		"remove a wider binding":          policyRequest(rev, allRoles, `{"kind":"email","role":"admin","email":"mine@example.test"}`, "null"),
		"narrow a wider role":             policyRequest(rev, narrowOwner, ownerBound+`,{"kind":"email","role":"admin","email":"mine@example.test"}`, "null"),
		"move a binding off a wider role": policyRequest(rev, allRoles, `{"kind":"email","role":"admin","email":"owner@example.test"}`, "null"),
		"clear the policy":                `{"action":"clear-policy","expectedPolicyRevision":` + rev + `}`,
		"invite with a wider role":        `{"action":"invite-user","email":"mine@example.test","roles":["owner"]}`,
		"give a person a wider role":      `{"action":"replace-user-roles","email":"small@example.test","roles":["owner"]}`,
		"re-role a wider person":          `{"action":"replace-user-roles","email":"wide@example.test","roles":["admin"]}`,
		"disable a wider person":          `{"action":"disable-user","email":"wide@example.test"}`,
		"enable a wider person":           `{"action":"enable-user","email":"wide@example.test"}`,
		"remove a wider person":           `{"action":"remove-user","email":"wide@example.test"}`,
		"register a provider":             `{"action":"upsert-oidc","registration":` + oidc + `,"origin":"https://lb.example.test","permissions":[]}`,
		"replace the provider":            cloudflare,
		"remove a registration":           `{"action":"remove-registration","registrationId":"google"}`,
		"turn off sign-in":                `{"action":"disable"}`,
		"point email delivery elsewhere":  `{"action":"configure-email-delivery","config":` + mailConfig + `}`,
		"turn off email delivery":         `{"action":"disable-email-delivery"}`,
	} {
		t.Run(name, func(t *testing.T) {
			got, code, out := f.post(delegate, accessRoute, body)
			if got != 403 || code != exceeds {
				t.Errorf("%d %s", got, out)
			}
			if f.state() != before {
				t.Error("a refused change changed something")
			}
		})
	}
	if _, err := os.Stat(f.s.Root.Dir + "/email-delivery.json"); err == nil {
		t.Error("a refused change wrote email delivery")
	}
}

// Whatever it holds, it can manage. A wider role it leaves alone stays as it is.
func TestAccessChangesWithinTheCallerStillWork(t *testing.T) {
	f := newAccessFixture(t)
	_, delegate := f.key("delegate", 0, "access:manage", "access:read", "inference:chat")
	withBinding := ownerBound + `,{"kind":"email","role":"chat","email":"mine@example.test"}`
	f.access(delegate, policyRequest(f.revision(), allRoles, withBinding, "null"), 200, "")
	f.access(delegate, policyRequest(f.revision(), allRoles+`,{"name":"mine","description":"","permissions":["inference:chat"]}`, withBinding, "null"), 200, "")
	f.access(delegate, policyRequest(f.revision(), allRoles+`,{"name":"mine","description":"changed words","permissions":["inference:chat"]}`, withBinding, `"chat"`), 200, "")
	f.access(delegate, policyRequest(f.revision(), allRoles, withBinding, "null"), 200, "")
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

// A person's permissions are all they resolve to, not only the roles on their record.
func TestAccessCountsEverythingAPersonResolvesTo(t *testing.T) {
	f := newAccessFixture(t)
	_, delegate := f.key("delegate", 0, "access:manage", "access:read", "inference:chat")
	for name, bindings := range map[string]string{
		"email":  ownerBound + `,{"kind":"email","role":"owner","email":"small@example.test"}`,
		"domain": ownerBound + `,{"kind":"email-domain","role":"owner","domain":"example.test"}`,
	} {
		t.Run(name, func(t *testing.T) {
			f.access(f.owner, policyRequest(f.revision(), allRoles, bindings, "null"), 200, "")
			f.access(delegate, `{"action":"disable-user","email":"small@example.test"}`, 403, exceeds)
			f.access(f.owner, policyRequest(f.revision(), allRoles, ownerBound, "null"), 200, "")
		})
	}
	t.Run("default role", func(t *testing.T) {
		f.access(f.owner, policyRequest(f.revision(), allRoles, ownerBound, `"owner"`), 200, "")
		f.access(delegate, `{"action":"disable-user","email":"small@example.test"}`, 403, exceeds)
		// Changing a wider default is refused, too.
		f.access(delegate, policyRequest(f.revision(), allRoles, ownerBound, `"chat"`), 403, exceeds)
		f.access(f.owner, policyRequest(f.revision(), allRoles, ownerBound, "null"), 200, "")
	})
	// With no such reach, the same action works.
	f.access(delegate, `{"action":"disable-user","email":"small@example.test"}`, 200, "")
}

// Without a policy, everyone signing in holds the provider-wide permissions. A first
// policy replaces them, and clearing a policy brings them back for everyone.
func TestPolicyAndProviderWidePermissions(t *testing.T) {
	f := &accessFixture{keyFixture: newKeyFixture(t)}
	_, f.owner = f.key("owner", 0, auth.Permissions...)
	f.access(f.owner, strings.Replace(cloudflare, `"permissions":["inference:chat"]`, `"permissions":["inference:chat","models:manage"]`, 1), 200, "")
	_, delegate := f.key("delegate", 0, "access:manage", "access:read", "inference:chat")
	first := policyRequest("null", chatRole+","+adminRole, `{"kind":"email","role":"admin","email":"mine@example.test"}`, "null")
	f.access(delegate, first, 403, exceeds)
	f.access(f.owner, first, 200, "")
	// It holds both roles' permissions, but not the provider-wide models:manage.
	_, narrow := f.key("narrow", 0, "access:manage", "access:read", "inference:chat")
	f.access(narrow, `{"action":"clear-policy","expectedPolicyRevision":`+f.revision()+`}`, 403, exceeds)
	// Setting provider-wide permissions past the caller is a grant to everyone.
	_, signIn := f.key("sign-in", 0, "access:manage", "access:read", "inference:chat", "models:manage")
	f.access(signIn, strings.Replace(cloudflare, `"permissions":["inference:chat"]`, `"permissions":["system:manage"]`, 1), 403, exceeds)
	f.access(signIn, strings.Replace(cloudflare, `"permissions":["inference:chat"]`, `"permissions":["models:manage"]`, 1), 200, "")
}

// A session is held to what its role gives it on this request.
func TestAccessRulesThroughARealSession(t *testing.T) {
	f := newKeyFixture(t)
	post, setRole := f.sessionPoster("browser:person-hash", "access:manage", "access:read")
	postAccess := func(body string) (int, string) {
		t.Helper()
		return post(body, accessRoute)
	}
	if status, out := postAccess(`{"action":"invite-user","email":"x@example.com","roles":["boss"]}`); status != 201 {
		t.Fatalf("a role within its own: %d %s", status, out)
	}
	// Grow the boss role past the session, which is held to its role alone.
	if err := auth.ApplyPolicy(context.Background(), f.st, &auth.Policy{
		Roles:    []auth.Role{{Name: "boss", Permissions: []string{"access:manage", "keys:manage"}}, {Name: "holder", Permissions: []string{"access:manage", "access:read"}}},
		Bindings: []auth.Binding{{Kind: "email", Role: "boss", Email: "boss@example.com"}, {Kind: "email", Role: "holder", Email: "person@example.com"}},
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
		res := f.s.Call(context.Background(), stale, "POST", accessRoute, "application/json", []byte(body))
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

// A key change and an access change share one lock: each check reads what the other
// writes, so neither may run between the other's check and its write.
func TestKeyAndAccessChangesCannotRaceToNobody(t *testing.T) {
	f := &accessFixture{keyFixture: newKeyFixture(t)}
	ctx := context.Background()
	host := &auth.Principal{Kind: "environment", Permissions: auth.Permissions}
	call := func(path, body string) int {
		return f.s.Call(ctx, host, "POST", path, "application/json", []byte(body)).Status
	}
	if got := call(accessRoute, cloudflare); got != 200 {
		t.Fatal(got)
	}
	const bound = `{"kind":"email","role":"admin","email":"person@example.test"}`
	if got := call(accessRoute, policyRequest("null", chatRole+","+adminRole, bound, "null")); got != 200 {
		t.Fatal(got)
	}
	if got := call(accessRoute, `{"action":"invite-user","email":"person@example.test","roles":["chat"]}`); got != 201 {
		t.Fatal(got)
	}
	// One key and one person can manage access. Each is being removed by the other side.
	k, _ := f.key("manager", 0, "access:manage")
	var arrived sync.WaitGroup
	arrived.Add(2)
	release := make(chan struct{})
	go func() { arrived.Wait(); close(release) }()
	f.s.beforeAdminWrite = func() {
		arrived.Done()
		select {
		case <-release:
		case <-time.After(200 * time.Millisecond):
			arrived.Add(1)
		}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); call(keysRoute, keyAction("revoke", k.ID)) }()
	go func() {
		defer wg.Done()
		call(accessRoute, `{"action":"disable-user","email":"person@example.test"}`)
	}()
	wg.Wait()
	g, err := auth.LoadGuard(ctx, f.st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !g.HasManager() {
		t.Fatal("the two changes together left nobody able to manage access")
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
