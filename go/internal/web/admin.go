package web

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

// Admin (§15 step 4, DESIGN §15.3): people and roles, browser access and API
// keys, ported from TS auth-management.tsx. Every read and change goes
// through /_localbase/access-management and /_localbase/api-keys as the
// signed-in person, so the API's permissions, validation and lockout rules
// apply; the page renders and confirms. Secrets and invitation links appear
// once, in the response to the change that made them.

// permissionGroups are TS's permission picker groups, in order.
var permissionGroups = []struct {
	Label, Prefix string
}{{"Inference", "inference:"}, {"Models", "models:"}, {"Configuration", "configuration:"}, {"API keys", "keys:"},
	{"Browser access", "access:"}, {"Sessions", "sessions:"}, {"System", "system:"}}

type permGroup struct {
	Label string
	Perms []permOption
}

type permOption struct {
	ID      string
	Checked bool
}

// pickerGroups is the picker with a selection.
func pickerGroups(selected []string) []permGroup {
	var out []permGroup
	for _, g := range permissionGroups {
		pg := permGroup{Label: g.Label}
		for _, p := range auth.Permissions {
			if strings.HasPrefix(p, g.Prefix) {
				pg.Perms = append(pg.Perms, permOption{ID: p, Checked: slices.Contains(selected, p)})
			}
		}
		if len(pg.Perms) > 0 {
			out = append(out, pg)
		}
	}
	return out
}

// formPermissions keeps the known permissions a form checked, in canonical order.
func formPermissions(r *http.Request, name string) []string {
	got := r.PostForm[name]
	out := []string{}
	for _, p := range auth.Permissions {
		if slices.Contains(got, p) {
			out = append(out, p)
		}
	}
	return out
}

type registration struct {
	Kind                 string `json:"kind"`
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	Issuer               string `json:"issuer"`
	ClientID             string `json:"clientId"`
	ClientAuthentication string `json:"clientAuthentication"`
}

type accessState struct {
	Config *struct {
		Origin      string   `json:"origin"`
		Permissions []string `json:"permissions"`
		Provider    struct {
			Kind          string         `json:"kind"`
			TeamDomain    string         `json:"teamDomain"`
			Audience      string         `json:"audience"`
			Registrations []registration `json:"registrations"`
		} `json:"provider"`
	} `json:"config"`
	EmailDelivery *struct {
		Host           string `json:"host"`
		Port           int    `json:"port"`
		Security       string `json:"security"`
		Authentication string `json:"authentication"`
		From           string `json:"from"`
	} `json:"emailDelivery"`
	Policy         *auth.Policy `json:"policy"`
	PolicyRevision *string      `json:"policyRevision"`
	Roles          []auth.Role  `json:"roles"`
	Users          []auth.User  `json:"users"`
}

type apiKey struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Prefix    string   `json:"prefix"`
	Scopes    []string `json:"scopes"`
	CreatedAt string   `json:"createdAt"`
	ExpiresAt *string  `json:"expiresAt"`
	RevokedAt *string  `json:"revokedAt"`
}

// Status is TS apiKeyStatus.
func (k apiKey) Status() string {
	if k.RevokedAt != nil {
		return "Revoked"
	}
	if k.ExpiresAt == nil {
		return "Active"
	}
	if t, err := time.Parse(time.RFC3339Nano, *k.ExpiresAt); err == nil && t.After(time.Now()) {
		return "Active"
	}
	return "Expired"
}

// apiCall runs an admin API request and decodes a success into out; it
// returns the API's message on failure.
func (u *UI) apiCall(r *http.Request, method, path string, body any, out any) string {
	var b []byte
	ct := ""
	if body != nil {
		b, _ = json.Marshal(body)
		ct = "application/json"
	}
	res := u.call(r, method, path, ct, b)
	if res.Status < 200 || res.Status >= 300 {
		if msg := res.apiError(); msg != "" {
			return msg
		}
		return "Request failed (" + strconv.Itoa(res.Status) + "). Try again."
	}
	if out != nil && json.Unmarshal(res.Body, out) != nil {
		return "The gateway returned an unexpected response."
	}
	return ""
}

// adminSections is TS administrationSections.
func adminSections(p *auth.Principal) []string {
	var out []string
	if p.Can("access:read") {
		out = append(out, "people", "access")
	}
	if p.Can("keys:read") {
		out = append(out, "keys")
	}
	return out
}

func (u *UI) admin(w http.ResponseWriter, r *http.Request) {
	p := u.principal(r)
	if !p.Authenticated() {
		u.need("access:read", nil)(w, r) // the sign-in flow
		return
	}
	sections := adminSections(p)
	if len(sections) == 0 {
		w.WriteHeader(http.StatusForbidden)
		u.render(w, r, "forbidden", map[string]any{"Permission": "access:read or keys:read"})
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, p))
	section := r.URL.Query().Get("section")
	if !slices.Contains(sections, section) {
		section = sections[0]
	}
	u.render(w, r, "admin", u.adminData(r, section, map[string]any{}))
}

// adminData loads what a section shows.
func (u *UI) adminData(r *http.Request, section string, data map[string]any) map[string]any {
	p := u.caller(r)
	data["Section"], data["Sections"] = section, adminSections(p)
	data["CanManageAccess"], data["CanManageKeys"] = p.Can("access:manage"), p.Can("keys:manage")
	if email := callerEmail(p); email != "" {
		data["VerifiedEmail"] = email
	}
	switch section {
	case "people", "access":
		var st accessState
		if msg := u.apiCall(r, "GET", "/_localbase/access-management", nil, &st); msg != "" {
			data["LoadError"] = msg
			break
		}
		data["DefaultRole"] = ""
		data["State"] = &st
		data["Users"] = u.userViews(&st, p)
		data["RoleViews"] = roleViews(&st)
		data["InviteRoles"] = inviteRoles(&st)
		if st.Policy != nil && st.Policy.DefaultRole != nil {
			data["DefaultRole"] = *st.Policy.DefaultRole
		}
		if st.Policy != nil {
			b, _ := json.MarshalIndent(st.Policy, "", "  ")
			data["PolicyJSON"] = string(b)
		}
		perms := []string{"inference:chat", "models:read"} // TS defaultBrowserPermissions
		if st.Config != nil {
			perms = st.Config.Permissions
		}
		data["ProviderPerms"] = pickerGroups(perms)
		data["NewRolePerms"] = pickerGroups([]string{"inference:chat", "models:read"})
		data["Edit"] = editRegistration(&st, r.URL.Query().Get("edit"), r.PostFormValue("registrationId"))
	case "keys":
		var res struct {
			Keys []apiKey `json:"keys"`
		}
		if msg := u.apiCall(r, "GET", "/_localbase/api-keys", nil, &res); msg != "" {
			data["LoadError"] = msg
			break
		}
		var keys []map[string]any
		for _, k := range res.Keys {
			keys = append(keys, map[string]any{"Key": k, "Status": k.Status(), "Picker": pickerGroups(k.Scopes)})
		}
		data["Keys"] = keys
		data["NewKeyPerms"] = pickerGroups([]string{"inference:chat", "models:read"})
	}
	return data
}

// callerEmail is the signed-in person's verified email, if any.
func callerEmail(p *auth.Principal) string {
	if p != nil && p.Kind == "browser-session" && p.Identity != nil {
		return p.Identity.VerifiedEmail
	}
	return ""
}

type userView struct {
	auth.User
	Self  bool
	Added string
	Roles []roleOption
}

type roleOption struct {
	Name, Description string
	Checked           bool
}

func (u *UI) userViews(st *accessState, p *auth.Principal) []userView {
	email := callerEmail(p)
	var out []userView
	for _, usr := range st.Users {
		v := userView{User: usr, Self: email != "" && strings.EqualFold(email, usr.Email)}
		if t, err := time.Parse(time.RFC3339Nano, usr.CreatedAt); err == nil {
			v.Added = t.Format("Jan 2, 2006")
		}
		for _, role := range st.Roles {
			v.Roles = append(v.Roles, roleOption{Name: role.Name, Description: role.Description, Checked: slices.Contains(usr.Roles, role.Name)})
		}
		out = append(out, v)
	}
	return out
}

type roleView struct {
	auth.Role
	Picker []permGroup
}

func roleViews(st *accessState) []roleView {
	var out []roleView
	for _, role := range st.Roles {
		out = append(out, roleView{Role: role, Picker: pickerGroups(role.Permissions)})
	}
	return out
}

// inviteRoles is TS reconcileInviteRoles with nothing chosen yet: the
// default role, else the first.
func inviteRoles(st *accessState) []roleOption {
	pick := ""
	if st.Policy != nil && st.Policy.DefaultRole != nil {
		pick = *st.Policy.DefaultRole
	} else if len(st.Roles) > 0 {
		pick = st.Roles[0].Name
	}
	var out []roleOption
	for _, role := range st.Roles {
		out = append(out, roleOption{Name: role.Name, Description: role.Description, Checked: role.Name == pick})
	}
	return out
}

// editView is the provider form's values (TS hydrate / editRegistration).
type editView struct {
	Provider                        string
	Origin                          string
	TeamDomain, Audience            string
	ID, Name, Issuer, ClientID      string
	PublicClient, Existing, Editing bool
}

func editRegistration(st *accessState, edit, posted string) editView {
	v := editView{Provider: "cloudflare-access"}
	if st.Config == nil {
		return v
	}
	v.Origin = st.Config.Origin
	prov := st.Config.Provider
	if prov.Kind == "cloudflare-access" {
		v.TeamDomain, v.Audience = prov.TeamDomain, prov.Audience
		return v
	}
	if edit == "" {
		edit = posted
	}
	if edit == "new" {
		v.Provider, v.Editing = "oidc", true
		return v
	}
	for _, reg := range prov.Registrations {
		if edit == "" || reg.ID == edit {
			v.Provider, v.ID, v.Name, v.Issuer, v.ClientID = reg.Kind, reg.ID, reg.Name, reg.Issuer, reg.ClientID
			v.PublicClient, v.Existing, v.Editing = reg.ClientAuthentication == "none", true, edit != ""
			return v
		}
	}
	return v
}

// starterPolicy is TS starterAccessPolicy.
func starterPolicy(email string) map[string]any {
	return map[string]any{
		"roles": []map[string]any{
			{"name": "admin", "description": "Full LocalBase administration", "permissions": auth.Permissions},
			{"name": "member", "description": "Chat and model discovery", "permissions": []string{"inference:chat", "models:read"}},
		},
		"bindings":    []map[string]any{{"kind": "email", "role": "admin", "email": email}},
		"defaultRole": "member",
	}
}

// adminFragment redraws a section after a change.
func (u *UI) adminFragment(w http.ResponseWriter, r *http.Request, section string, extra map[string]any) {
	w.Header().Set("Cache-Control", "no-store")
	r.Header.Set("HX-Request", "true")
	r.Header.Set("HX-Target", "admin-section")
	u.render(w, r, "admin", u.adminData(r, section, extra))
}

func revisionValue(s string) any {
	if s == "" || s == "null" {
		return nil
	}
	return s
}

// adminAccess runs a people, roles or browser-access change.
func (u *UI) adminAccess(w http.ResponseWriter, r *http.Request) {
	action, section := r.PostFormValue("action"), r.PostFormValue("section")
	if section != "people" {
		section = "access"
	}
	f := r.PostFormValue
	extra := map[string]any{}
	done := func(notice, failure string) {
		if failure != "" {
			extra["Error"] = failure
		} else {
			extra["Notice"] = notice
		}
		u.adminFragment(w, r, section, extra)
	}
	post := func(body map[string]any, out any) string {
		return u.apiCall(r, "POST", "/_localbase/access-management", body, out)
	}
	// Role changes edit the current policy (TS applyPolicy).
	withPolicy := func(change func(p *auth.Policy) error, notice string) {
		var st accessState
		if msg := u.apiCall(r, "GET", "/_localbase/access-management", nil, &st); msg != "" {
			done("", msg)
			return
		}
		if st.Policy == nil {
			done("", "Configure an access policy before creating roles.")
			return
		}
		next := *st.Policy
		next.Roles = slices.Clone(next.Roles)
		if err := change(&next); err != nil {
			done("", err.Error())
			return
		}
		var rev any
		if st.PolicyRevision != nil {
			rev = *st.PolicyRevision
		}
		done(notice, post(map[string]any{"action": "apply-policy", "policy": next, "expectedPolicyRevision": rev}, nil))
	}
	switch action {
	case "save-provider":
		provider := f("provider")
		body := map[string]any{"origin": f("origin"), "permissions": formPermissions(r, "permissions")}
		switch provider {
		case "cloudflare-access":
			body["action"] = "configure-cloudflare"
			body["provider"] = map[string]any{"kind": provider, "teamDomain": f("teamDomain"), "audience": f("audience")}
		case "oidc":
			clientAuth := map[string]any{"kind": "client-secret-basic", "clientSecret": f("clientSecret")}
			if f("publicClient") == "1" {
				clientAuth = map[string]any{"kind": "none"}
			}
			body["action"] = "upsert-oidc"
			body["registration"] = map[string]any{"kind": provider, "id": f("registrationId"), "name": f("registrationName"), "issuer": f("issuer"),
				"clientId": f("clientId"), "clientAuthentication": clientAuth}
		case "github-oauth":
			body["action"] = "upsert-github"
			body["registration"] = map[string]any{"kind": provider, "id": f("registrationId"), "name": f("registrationName"), "clientId": f("clientId"), "clientSecret": f("clientSecret")}
		case "magic-link":
			body["action"] = "upsert-magic-link"
			body["registration"] = map[string]any{"kind": provider, "id": f("registrationId"), "name": f("registrationName")}
		default:
			done("", "Choose a sign-in provider.")
			return
		}
		done("Saved. Restart LocalBase to apply browser access changes.", post(body, nil))
	case "remove-registration":
		var res struct {
			Config *json.RawMessage `json:"config"`
		}
		msg := post(map[string]any{"action": "remove-registration", "registrationId": f("registrationId")}, &res)
		notice := "Registration removed. Restart LocalBase to apply the change."
		if res.Config == nil || string(*res.Config) == "null" {
			notice = "Last registration removed. Browser access will be disabled after restart."
		}
		done(notice, msg)
	case "apply-policy":
		var policy json.RawMessage
		if json.Unmarshal([]byte(f("policy")), &policy) != nil {
			done("", "The policy is not valid JSON.")
			return
		}
		done("Policy saved. The change is active now.", post(map[string]any{"action": "apply-policy", "policy": policy, "expectedPolicyRevision": revisionValue(f("revision"))}, nil))
	case "clear-policy":
		done("Policy cleared. Provider-wide permissions now apply.", post(map[string]any{"action": "clear-policy", "expectedPolicyRevision": revisionValue(f("revision"))}, nil))
	case "starter-policy":
		email := callerEmail(u.caller(r))
		if email == "" {
			done("", "Your identity provider did not supply a verified email. Configure the policy with Advanced JSON or the CLI.")
			return
		}
		done("Access policy created. Your verified email has the admin role.", post(map[string]any{"action": "apply-policy", "policy": starterPolicy(email), "expectedPolicyRevision": nil}, nil))
	case "invite-user":
		var st accessState
		u.apiCall(r, "GET", "/_localbase/access-management", nil, &st)
		send := f("sendEmail") == "1" && st.EmailDelivery != nil // TS shouldSendInvitationEmail
		var res struct {
			SignInURL      string `json:"signInUrl"`
			EmailDelivered bool   `json:"emailDelivered"`
		}
		msg := post(map[string]any{"action": "invite-user", "email": f("email"), "roles": r.PostForm["roles"], "sendEmail": send}, &res)
		if msg == "" {
			extra["InviteURL"] = res.SignInURL
		}
		notice := "User invited. Copy the sign-in link."
		switch {
		case res.EmailDelivered:
			notice = "User invited and email sent."
		case send:
			notice = "User invited, but email delivery failed. Copy the sign-in link."
		}
		done(notice, msg)
	case "replace-user-roles", "enable-user", "disable-user", "remove-user":
		email := f("email")
		body := map[string]any{"action": action, "email": email}
		if action == "replace-user-roles" {
			roles := r.PostForm["roles"]
			if roles == nil {
				roles = []string{}
			}
			body["roles"] = roles
		}
		notice := map[string]string{"remove-user": email + " removed.", "replace-user-roles": "Roles saved for " + email + ".",
			"enable-user": email + " enabled.", "disable-user": email + " disabled."}[action]
		done(notice, post(body, nil))
	case "create-role":
		role := auth.Role{Name: strings.TrimSpace(f("name")), Description: f("description"), Permissions: formPermissions(r, "permissions")}
		withPolicy(func(p *auth.Policy) error { p.Roles = append(p.Roles, role); return nil }, "Role "+role.Name+" created.")
	case "save-role":
		role := auth.Role{Name: f("name"), Description: f("description"), Permissions: formPermissions(r, "permissions")}
		withPolicy(func(p *auth.Policy) error {
			for i := range p.Roles {
				if p.Roles[i].Name == role.Name {
					p.Roles[i] = role
				}
			}
			return nil
		}, "Role "+role.Name+" saved.")
	case "remove-role":
		name := f("name")
		withPolicy(func(p *auth.Policy) error {
			p.Roles = slices.DeleteFunc(p.Roles, func(x auth.Role) bool { return x.Name == name })
			return nil
		}, "Role "+name+" removed.")
	case "default-role":
		name := f("name")
		notice := "Eligible signed-in identities now receive " + name + "."
		if name == "" {
			notice = "Default role cleared."
		}
		withPolicy(func(p *auth.Policy) error {
			if name == "" {
				p.DefaultRole = nil
			} else {
				p.DefaultRole = &name
			}
			return nil
		}, notice)
	case "configure-email-delivery":
		port, _ := strconv.Atoi(f("port"))
		authn := map[string]any{"kind": "none"}
		if f("passwordAuth") == "1" {
			authn = map[string]any{"kind": "password", "username": f("username"), "password": f("password")}
		}
		done("Email delivery saved.", post(map[string]any{"action": action, "config": map[string]any{"host": f("host"), "port": port,
			"security": f("security"), "authentication": authn, "from": f("from")}}, nil))
	case "test-email-delivery":
		done("Test email sent to "+f("to")+".", post(map[string]any{"action": action, "to": f("to")}, nil))
	case "disable-email-delivery":
		done("Email delivery disabled.", post(map[string]any{"action": action}, nil))
	default:
		http.Error(w, "Bad Request", http.StatusBadRequest)
	}
}

// adminKeys creates, rotates, revokes or rescopes an API key.
func (u *UI) adminKeys(w http.ResponseWriter, r *http.Request) {
	action, f := r.PostFormValue("action"), r.PostFormValue
	extra := map[string]any{}
	var res struct {
		Secret string `json:"secret"`
	}
	var body map[string]any
	notice := ""
	switch action {
	case "create":
		body = map[string]any{"action": "create", "name": f("name"), "scopes": formPermissions(r, "scopes")}
		if days := strings.TrimSpace(f("expiresDays")); days != "" {
			n, err := strconv.Atoi(days)
			if err != nil {
				extra["Error"] = "Expiry must be a whole number of days."
				u.adminFragment(w, r, "keys", extra)
				return
			}
			body["expiresDays"] = n
		}
		notice = "API key created. Copy its secret now."
	case "rotate":
		body, notice = map[string]any{"action": "rotate", "keyId": f("keyId")}, "API key rotated. Copy its new secret now."
	case "revoke":
		body, notice = map[string]any{"action": "revoke", "keyId": f("keyId")}, "API key revoked."
	case "set-scopes":
		body, notice = map[string]any{"action": "set-scopes", "keyId": f("keyId"), "scopes": formPermissions(r, "scopes")}, "Scopes saved."
	default:
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if msg := u.apiCall(r, "POST", "/_localbase/api-keys", body, &res); msg != "" {
		extra["Error"] = msg
	} else {
		extra["Notice"] = notice
		if res.Secret != "" {
			extra["Secret"] = res.Secret // shown once, in this response only
		}
	}
	u.adminFragment(w, r, "keys", extra)
}
