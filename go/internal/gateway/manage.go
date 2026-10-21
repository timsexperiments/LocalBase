package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	rootpkg "github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Admin APIs (management-http.ts): /_localbase/access-management,
// /_localbase/api-keys, and /_localbase/hf-token.

var managementMessages = map[string]string{
	"invalid_api_key":               "Authentication is required.",
	"insufficient_permissions":      "This identity cannot perform that action.",
	"validation_failed":             "The management request is invalid.",
	"payload_too_large":             "The management request exceeds the size limit.",
	"request_aborted":               "The management request was cancelled.",
	"provider_not_configured":       "Configure a browser identity provider first.",
	"email_delivery_not_configured": "Configure email delivery before enabling magic-link authentication.",
	"email_delivery_in_use":         "Remove the magic-link registration before disabling email delivery.",
	"email_delivery_failed":         "Email delivery failed.",
	"policy_not_configured":         "Configure a browser access policy first.",
	"registration_not_found":        "The identity provider registration was not found.",
	"key_not_found":                 "The API key was not found.",
	"managed_user_not_found":        "The managed user was not found.",
	"managed_user_exists":           "The managed user already exists.",
	"role_not_found":                "One or more browser access roles were not found.",
	"policy_conflict":               "The browser access policy cannot be changed.",
	"policy_revision_conflict":      "The browser access policy changed. Refresh it and try again.",
}

func managementError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": managementMessages[code]}})
}

func noStore(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, v)
}

// principal resolves the caller for routes that do their own permission
// checks: a forwarded browser identity, --no-auth, or an API credential.
func (s *Server) principal(r *http.Request) *auth.Principal {
	if bp, ok := r.Context().Value(browserPrincipalKey{}).(*auth.Principal); ok {
		return bp
	}
	if s.NoAuth {
		return &auth.Principal{Kind: "environment", Permissions: auth.Permissions}
	}
	p, _ := s.Auth.ResolveAPI(r.Context(), r)
	setAuth(r, p)
	return p
}

const maxManagementBody = 64 * 1024

func readManagement(w http.ResponseWriter, r *http.Request) (json.RawMessage, bool) {
	if r.ContentLength > maxManagementBody {
		managementError(w, 413, "payload_too_large")
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxManagementBody+1))
	if err != nil {
		if r.Context().Err() != nil {
			managementError(w, 499, "request_aborted")
		} else {
			managementError(w, 400, "validation_failed")
		}
		return nil, false
	}
	if len(b) > maxManagementBody {
		managementError(w, 413, "payload_too_large")
		return nil, false
	}
	if !json.Valid(b) {
		managementError(w, 400, "validation_failed")
		return nil, false
	}
	return b, true
}

// strictDecode decodes JSON rejecting unknown fields.
func strictDecode(b []byte, v any) bool {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v) == nil
}

func (s *Server) domainError(w http.ResponseWriter, err error) bool {
	var ae *auth.AccessError
	if !errors.As(err, &ae) {
		return false
	}
	mapped := map[string][2]any{
		"policy-not-configured":    {"policy_not_configured", 409},
		"managed-user-not-found":   {"managed_user_not_found", 404},
		"managed-user-exists":      {"managed_user_exists", 409},
		"role-not-found":           {"role_not_found", 400},
		"policy-conflict":          {"policy_conflict", 409},
		"policy-revision-conflict": {"policy_revision_conflict", 409},
		"would-lock-out-admins":    {"would_lock_out_admins", 409},
	}[ae.Code]
	if ae.Code == "would-lock-out-admins" {
		// Go-only lockout rules: the error carries its own explanation.
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 409, map[string]any{"error": map[string]string{"code": mapped[0].(string), "message": ae.Message}})
		return true
	}
	managementError(w, mapped[1].(int), mapped[0].(string))
	return true
}

// --- api keys -----------------------------------------------------------------------

func publicKey(k *store.APIKey) map[string]any {
	scopes := k.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	m := map[string]any{"id": k.ID, "name": k.Name, "prefix": k.Prefix, "scopes": scopes, "createdAt": k.CreatedAt,
		"lastRotatedAt": k.LastRotatedAt}
	if k.ExpiresAt != nil {
		m["expiresAt"] = *k.ExpiresAt
	}
	if k.RevokedAt != nil {
		m["revokedAt"] = *k.RevokedAt
	}
	return m
}

func validScopes(scopes []string) ([]string, bool) {
	for _, sc := range scopes {
		if !contains(auth.Permissions, sc) {
			return nil, false
		}
	}
	return auth.Canonical(scopes), true
}

func (s *Server) apiKeys(w http.ResponseWriter, r *http.Request) {
	p := s.principal(r)
	if !p.Authenticated() {
		managementError(w, 401, "invalid_api_key")
		return
	}
	ctx := r.Context()
	if r.Method == http.MethodGet {
		if !p.Can("keys:read") {
			managementError(w, 403, "insufficient_permissions")
			return
		}
		keys, err := s.Store.ListAPIKeys(ctx)
		if err != nil {
			serviceUnavailable(w, r, "Key management")
			return
		}
		out := []map[string]any{}
		for i := range keys {
			out = append(out, publicKey(&keys[i]))
		}
		noStore(w, 200, map[string]any{"keys": out})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(405)
		return
	}
	if !p.Can("keys:manage") {
		managementError(w, 403, "insufficient_permissions")
		return
	}
	body, ok := readManagement(w, r)
	if !ok {
		return
	}
	var in struct {
		Action      string    `json:"action"`
		Name        *string   `json:"name"`
		ExpiresDays *float64  `json:"expiresDays"`
		Scopes      *[]string `json:"scopes"`
		KeyID       *string   `json:"keyId"`
	}
	if !strictDecode(body, &in) {
		managementError(w, 400, "validation_failed")
		return
	}
	keyID := ""
	if in.KeyID != nil {
		keyID = *in.KeyID
	}
	valid := true
	switch in.Action {
	case "create":
		valid = in.Name != nil && strings.TrimSpace(*in.Name) != "" && len(*in.Name) <= 128 && in.Scopes != nil && in.KeyID == nil &&
			(in.ExpiresDays == nil || (*in.ExpiresDays == float64(int(*in.ExpiresDays)) && *in.ExpiresDays >= 1 && *in.ExpiresDays <= 3650))
	case "rotate", "revoke":
		valid = keyID != "" && len(keyID) <= 128 && in.Name == nil && in.Scopes == nil && in.ExpiresDays == nil
	case "set-scopes":
		valid = keyID != "" && len(keyID) <= 128 && in.Scopes != nil && in.Name == nil && in.ExpiresDays == nil
	default:
		valid = false
	}
	valid = valid && onlyFields(body, map[string][]string{"create": {"name", "expiresDays", "scopes"}, "rotate": {"keyId"},
		"revoke": {"keyId"}, "set-scopes": {"keyId", "scopes"}}[in.Action]...)
	var scopes []string
	if valid && in.Scopes != nil {
		scopes, valid = validScopes(*in.Scopes)
	}
	if !valid {
		managementError(w, 400, "validation_failed")
		return
	}
	if in.Action != "create" {
		if _, err := s.Store.APIKeyByID(ctx, keyID); err != nil {
			managementError(w, 404, "key_not_found")
			return
		}
	}
	// Lockout rules (a Go addition, DESIGN §13.6): not your own key, and
	// never the last principal with access:manage.
	if in.Action == "revoke" || in.Action == "set-scopes" {
		var perms []string
		if cfg, err := auth.LoadAccess(s.Root.Dir); err == nil && cfg != nil {
			perms = cfg.Permissions
		}
		g, err := auth.LoadGuard(ctx, s.Store, perms)
		if err != nil {
			serviceUnavailable(w, r, "Key management")
			return
		}
		callerKey := ""
		if p.Kind == "api-key" {
			callerKey = p.ID
		}
		removes := in.Action == "revoke" || !slices.Contains(scopes, "access:manage")
		if err := g.CheckKeyChange(callerKey, keyID, removes, in.Action == "revoke"); err != nil {
			s.domainError(w, err)
			return
		}
	}
	switch in.Action {
	case "create":
		days := 0
		if in.ExpiresDays != nil {
			days = int(*in.ExpiresDays)
		}
		k, raw, err := s.Store.CreateAPIKeyExpiring(ctx, *in.Name, scopes, days)
		if err != nil {
			serviceUnavailable(w, r, "Key management")
			return
		}
		noStore(w, 201, map[string]any{"key": publicKey(k), "secret": raw})
	case "rotate":
		k, raw, err := s.Store.RotateAPIKey(ctx, keyID)
		if err != nil {
			serviceUnavailable(w, r, "Key management")
			return
		}
		noStore(w, 200, map[string]any{"key": publicKey(k), "secret": raw})
	case "revoke":
		k, err := s.Store.RevokeAPIKey(ctx, keyID)
		if err != nil {
			serviceUnavailable(w, r, "Key management")
			return
		}
		noStore(w, 200, map[string]any{"key": publicKey(k)})
	case "set-scopes":
		k, err := s.Store.SetAPIKeyScopes(ctx, keyID, scopes)
		if err != nil {
			serviceUnavailable(w, r, "Key management")
			return
		}
		noStore(w, 200, map[string]any{"key": publicKey(k)})
	}
}

// --- access management ------------------------------------------------------------------

func (s *Server) accessManagement(w http.ResponseWriter, r *http.Request) {
	p := s.principal(r)
	if !p.Authenticated() {
		managementError(w, 401, "invalid_api_key")
		return
	}
	ctx := r.Context()
	root := s.Root.Dir
	if r.Method == http.MethodGet {
		if !p.Can("access:read") {
			managementError(w, 403, "insufficient_permissions")
			return
		}
		cfg, err := auth.LoadAccess(root)
		policy, perr := auth.LoadPolicy(ctx, s.Store.DB)
		users, uerr := auth.ListUsers(ctx, s.Store)
		email, eerr := auth.LoadEmail(root)
		if err != nil || perr != nil || uerr != nil || eerr != nil {
			serviceUnavailable(w, r, "Access management")
			return
		}
		var summary, emailSummary any
		if cfg != nil {
			summary = cfg.Summary()
		}
		if email != nil {
			emailSummary = email.Summary()
		}
		roles := []auth.Role{}
		var policyOut any
		if policy != nil {
			roles, policyOut = policy.Roles, policy
		}
		noStore(w, 200, map[string]any{"config": summary, "policy": policyOut, "policyRevision": auth.Revision(policy),
			"users": users, "roles": roles, "emailDelivery": emailSummary})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(405)
		return
	}
	if !p.Can("access:read") && !p.Can("access:manage") {
		managementError(w, 403, "insufficient_permissions")
		return
	}
	body, ok := readManagement(w, r)
	if !ok {
		return
	}
	var head struct{ Action string }
	json.Unmarshal(body, &head)
	required := "access:manage"
	if head.Action == "test-policy" {
		required = "access:read"
	}
	in, valid := parseAccessRequest(body)
	if !valid {
		managementError(w, 400, "validation_failed")
		return
	}
	if !p.Can(required) {
		managementError(w, 403, "insufficient_permissions")
		return
	}
	if in.Action != "test-policy" && in.Action != "test-email-delivery" {
		op := "update browser access"
		if in.Action == "invite-user" {
			op = "invite browser user"
		}
		lease, err := rootpkg.AcquireOperation(root, op)
		if err != nil {
			serviceUnavailable(w, r, "Access management")
			return
		}
		defer lease.Release()
	}
	current, err := auth.LoadAccess(root)
	if err != nil {
		serviceUnavailable(w, r, "Access management")
		return
	}
	// lockout refuses a change that would lock the caller or everyone out of
	// access management (a Go addition; see auth.Guard). It reports whether
	// it wrote a response.
	lockout := func(email string, self bool, change func(*auth.Guard)) bool {
		var perms []string
		if current != nil {
			perms = current.Permissions
		}
		g, err := auth.LoadGuard(ctx, s.Store, perms)
		if err != nil {
			serviceUnavailable(w, r, "Access management")
			return true
		}
		var caller *auth.Identity
		if p.Kind == "browser-session" {
			caller = p.Identity
		}
		if self && g.IsCaller(caller, email) {
			s.domainError(w, auth.ErrSelfRecord)
			return true
		}
		if err := g.CheckChange(caller, change); err != nil {
			s.domainError(w, err)
			return true
		}
		return false
	}
	configured := func(cfg *auth.BrowserAccess) {
		noStore(w, 200, map[string]any{"config": cfg.Summary(), "restartRequired": true})
	}
	save := func(cfg *auth.BrowserAccess) bool {
		if err := auth.SaveAccess(root, cfg); err != nil {
			managementError(w, 400, "validation_failed")
			return false
		}
		return true
	}
	switch in.Action {
	case "test-policy":
		if current == nil {
			managementError(w, 409, "provider_not_configured")
			return
		}
		policy, _ := auth.LoadPolicy(ctx, s.Store.DB)
		d, err := auth.ResolvePolicy(ctx, s.Store, *in.Identity, false)
		if err != nil {
			serviceUnavailable(w, r, "Access management")
			return
		}
		if d == nil {
			d = &auth.Decision{MatchedRoles: []string{}, Permissions: current.Permissions}
		}
		noStore(w, 200, map[string]any{"policyConfigured": policy != nil, "matchedRoles": d.MatchedRoles, "permissions": d.Permissions})
	case "test-email-delivery":
		email, _ := auth.LoadEmail(root)
		if email == nil {
			managementError(w, 409, "email_delivery_not_configured")
			return
		}
		if err := auth.SendEmail(email, in.To, "LocalBase email delivery test", "LocalBase successfully sent this test email."); err != nil {
			managementError(w, 502, "email_delivery_failed")
			return
		}
		noStore(w, 200, map[string]any{"emailDelivered": true, "to": in.To})
	case "invite-user":
		if current == nil {
			managementError(w, 409, "provider_not_configured")
			return
		}
		var email *auth.EmailConfig
		if in.SendEmail {
			if email, _ = auth.LoadEmail(root); email == nil {
				managementError(w, 409, "email_delivery_not_configured")
				return
			}
		}
		user, err := auth.InviteUser(ctx, s.Store, in.Email, in.Roles)
		if err != nil {
			if !s.domainError(w, err) {
				managementError(w, 400, "validation_failed")
			}
			return
		}
		signIn := current.Origin + "/app/login"
		delivered := false
		if email != nil {
			delivered = auth.SendEmail(email, user.Email, "You were invited to LocalBase",
				"You were invited to LocalBase. Sign in here:\n\n"+signIn) == nil
		}
		noStore(w, 201, map[string]any{"user": user, "signInUrl": signIn, "emailDelivered": delivered})
	case "configure-cloudflare":
		cfg := &auth.BrowserAccess{Provider: *in.Provider, Origin: in.Origin, Permissions: in.Permissions}
		if save(cfg) {
			configured(cfg)
		}
	case "upsert-oidc", "upsert-github", "upsert-magic-link":
		if in.Action == "upsert-magic-link" {
			if email, _ := auth.LoadEmail(root); email == nil {
				managementError(w, 409, "email_delivery_not_configured")
				return
			}
			if current != nil && current.Provider.Kind == "direct" {
				for _, reg := range current.Provider.Registrations {
					if reg.Kind == "magic-link" && reg.ID != in.Registration.ID {
						managementError(w, 400, "validation_failed")
						return
					}
				}
			}
		}
		cfg := auth.UpsertRegistration(current, *in.Registration, in.Origin, in.Permissions)
		if save(cfg) {
			configured(cfg)
		}
	case "remove-registration":
		kind, cfg := auth.RemoveRegistration(current, in.RegistrationID)
		switch kind {
		case "not-found":
			managementError(w, 404, "registration_not_found")
			return
		case "disabled":
			auth.DisableAccess(root)
			noStore(w, 200, map[string]any{"removedRegistrationId": in.RegistrationID, "config": nil, "restartRequired": true})
		default:
			if save(cfg) {
				noStore(w, 200, map[string]any{"removedRegistrationId": in.RegistrationID, "config": cfg.Summary(), "restartRequired": true})
			}
		}
	case "disable":
		disabled, _ := auth.DisableAccess(root)
		noStore(w, 200, map[string]any{"disabled": disabled, "restartRequired": disabled})
	case "configure-email-delivery":
		if err := auth.SaveEmail(root, in.Config); err != nil {
			managementError(w, 400, "validation_failed")
			return
		}
		noStore(w, 200, map[string]any{"emailDelivery": in.Config.Summary(), "restartRequired": false})
	case "disable-email-delivery":
		if current != nil && current.Provider.Kind == "direct" {
			for _, reg := range current.Provider.Registrations {
				if reg.Kind == "magic-link" {
					managementError(w, 409, "email_delivery_in_use")
					return
				}
			}
		}
		disabled, _ := auth.DisableEmail(root)
		noStore(w, 200, map[string]any{"emailDeliveryDisabled": disabled, "restartRequired": false})
	case "replace-user-roles", "enable-user", "disable-user", "remove-user":
		var user *auth.User
		var err error
		if in.Action != "enable-user" && lockout(in.Email, in.Action != "replace-user-roles", func(next *auth.Guard) {
			u := next.User(in.Email)
			switch {
			case u == nil:
			case in.Action == "replace-user-roles":
				u.Roles = in.Roles
			case in.Action == "disable-user":
				u.Status = "disabled"
			default:
				// Forgetting deletes the record: bindings apply again on sign-in.
				next.Users = slices.DeleteFunc(next.Users, func(x auth.GuardUser) bool { return x.ID == u.ID })
			}
		}) {
			return
		}
		switch in.Action {
		case "replace-user-roles":
			user, err = auth.ReplaceUserRoles(ctx, s.Store, in.Email, in.Roles)
		case "enable-user":
			user, err = auth.SetUserStatus(ctx, s.Store, in.Email, "active")
		case "disable-user":
			user, err = auth.SetUserStatus(ctx, s.Store, in.Email, "disabled")
		default:
			user, err = auth.RemoveUser(ctx, s.Store, in.Email)
		}
		if err != nil {
			if !s.domainError(w, err) {
				managementError(w, 400, "validation_failed")
			}
			return
		}
		noStore(w, 200, map[string]any{"user": user})
	case "apply-policy", "clear-policy":
		if in.Action == "apply-policy" && current == nil {
			managementError(w, 409, "provider_not_configured")
			return
		}
		existing, err := auth.LoadPolicy(ctx, s.Store.DB)
		if err != nil {
			serviceUnavailable(w, r, "Access management")
			return
		}
		rev := auth.Revision(existing)
		if (rev == nil) != (in.ExpectedRevision == nil) || (rev != nil && *rev != *in.ExpectedRevision) {
			managementError(w, 409, "policy_revision_conflict")
			return
		}
		if lockout("", false, func(next *auth.Guard) {
			if in.Action == "clear-policy" {
				// Managed users block clearing (TS refuses with policy_conflict);
				// leave that refusal to ClearPolicy.
				if len(next.Users) == 0 {
					next.Policy = nil
				}
				return
			}
			// Judge the policy as it will be stored (normalized); an invalid
			// one is left for ApplyPolicy to reject.
			if in.Policy == nil {
				return
			}
			candidate := *in.Policy
			candidate.Roles = slices.Clone(candidate.Roles)
			candidate.Bindings = slices.Clone(candidate.Bindings)
			if candidate.Normalize() == nil {
				next.Policy = &candidate
			}
		}) {
			return
		}
		if in.Action == "clear-policy" {
			cleared, err := auth.ClearPolicy(ctx, s.Store)
			if err != nil {
				if !s.domainError(w, err) {
					serviceUnavailable(w, r, "Access management")
				}
				return
			}
			noStore(w, 200, map[string]any{"cleared": cleared, "policyRevision": nil, "restartRequired": false})
			return
		}
		if err := auth.ApplyPolicy(ctx, s.Store, in.Policy); err != nil {
			if !s.domainError(w, err) {
				managementError(w, 400, "validation_failed")
			}
			return
		}
		noStore(w, 200, map[string]any{"policy": in.Policy, "policyRevision": auth.Revision(in.Policy), "restartRequired": false})
	}
}

type accessRequest struct {
	Action           string             `json:"action"`
	Provider         *auth.Provider     `json:"provider"`
	Registration     *auth.Registration `json:"registration"`
	Origin           string             `json:"origin"`
	Permissions      []string           `json:"permissions"`
	RegistrationID   string             `json:"registrationId"`
	Config           *auth.EmailConfig  `json:"config"`
	To               string             `json:"to"`
	Email            string             `json:"email"`
	Roles            []string           `json:"roles"`
	SendEmail        bool               `json:"sendEmail"`
	Policy           *auth.Policy       `json:"policy"`
	ExpectedRevision *string            `json:"expectedPolicyRevision"`
	Identity         *auth.Identity     `json:"identity"`
	raw              map[string]json.RawMessage
}

var revisionRe = regexp.MustCompile(`^[a-f0-9]{64}$`)

// parseAccessRequest applies the per-action TS schema: exactly the action's
// fields, each valid.
func parseAccessRequest(b []byte) (*accessRequest, bool) {
	var in accessRequest
	if !strictDecode(b, &in) {
		return nil, false
	}
	json.Unmarshal(b, &in.raw)
	allowed := map[string][]string{
		"configure-cloudflare":     {"provider", "origin", "permissions"},
		"upsert-oidc":              {"registration", "origin", "permissions"},
		"upsert-github":            {"registration", "origin", "permissions"},
		"upsert-magic-link":        {"registration", "origin", "permissions"},
		"remove-registration":      {"registrationId"},
		"disable":                  {},
		"configure-email-delivery": {"config"},
		"disable-email-delivery":   {},
		"test-email-delivery":      {"to"},
		"invite-user":              {"email", "roles", "sendEmail"},
		"replace-user-roles":       {"email", "roles"},
		"enable-user":              {"email"},
		"disable-user":             {"email"},
		"remove-user":              {"email"},
		"apply-policy":             {"policy", "expectedPolicyRevision"},
		"clear-policy":             {"expectedPolicyRevision"},
		"test-policy":              {"identity"},
	}
	fields, ok := allowed[in.Action]
	if !ok {
		return nil, false
	}
	for k, v := range in.raw {
		if k != "action" && !contains(fields, k) {
			return nil, false
		}
		if string(v) == "null" && !(in.Action == "apply-policy" && k == "expectedPolicyRevision") {
			return nil, false
		}
	}
	has := func(k string) bool { _, ok := in.raw[k]; return ok }
	switch in.Action {
	case "configure-cloudflare", "upsert-oidc", "upsert-github", "upsert-magic-link":
		if !auth.ValidOrigin(in.Origin) || !has("permissions") {
			return nil, false
		}
		if _, ok := validScopes(in.Permissions); !ok {
			return nil, false
		}
		kind := map[string]string{"upsert-oidc": "oidc", "upsert-github": "github-oauth", "upsert-magic-link": "magic-link"}[in.Action]
		if in.Action == "configure-cloudflare" {
			if in.Provider == nil || in.Provider.Kind != "cloudflare-access" {
				return nil, false
			}
			probe := &auth.BrowserAccess{Provider: *in.Provider, Origin: in.Origin, Permissions: in.Permissions}
			if probe.Validate() != nil {
				return nil, false
			}
		} else {
			if in.Registration == nil || in.Registration.Kind != kind {
				return nil, false
			}
			probe := &auth.BrowserAccess{Provider: auth.Provider{Kind: "direct", Registrations: []auth.Registration{*in.Registration}},
				Origin: in.Origin, Permissions: in.Permissions}
			if probe.Validate() != nil {
				return nil, false
			}
		}
	case "remove-registration":
		if in.RegistrationID == "" || len(in.RegistrationID) > 64 {
			return nil, false
		}
	case "configure-email-delivery":
		if in.Config == nil || in.Config.Validate() != nil {
			return nil, false
		}
	case "test-email-delivery":
		if auth.ValidEmail(in.To) == "" {
			return nil, false
		}
	case "invite-user", "replace-user-roles", "enable-user", "disable-user", "remove-user":
		if auth.ValidEmail(in.Email) == "" || (in.Action == "invite-user" && len(in.Roles) == 0) ||
			((in.Action == "invite-user" || in.Action == "replace-user-roles") && in.Roles == nil) {
			return nil, false
		}
	case "apply-policy":
		if in.Policy == nil || !has("expectedPolicyRevision") || (in.ExpectedRevision != nil && !revisionRe.MatchString(*in.ExpectedRevision)) {
			return nil, false
		}
		copyPolicy := *in.Policy
		if copyPolicy.Normalize() != nil {
			return nil, false
		}
	case "clear-policy":
		if in.ExpectedRevision == nil || !revisionRe.MatchString(*in.ExpectedRevision) {
			return nil, false
		}
	case "test-policy":
		if in.Identity == nil || in.Identity.Issuer == "" || in.Identity.Subject == "" || len(in.Identity.Subject) > 512 ||
			(in.Identity.VerifiedEmail != "" && auth.ValidEmail(in.Identity.VerifiedEmail) == "") {
			return nil, false
		}
	}
	return &in, true
}

// --- hugging face token -----------------------------------------------------------------

var hfTokenRe = regexp.MustCompile(`^[\x21-\x7e]{8,256}$`)

// HFEndpoint is the Hugging Face base URL (HF_ENDPOINT overrides it).
func HFEndpoint() string {
	if v := strings.TrimRight(os.Getenv("HF_ENDPOINT"), "/"); v != "" {
		return v
	}
	return "https://huggingface.co"
}

// CheckHFToken asks the Hub who a token belongs to.
func CheckHFToken(ctx context.Context, token string) map[string]any {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, HFEndpoint()+"/api/whoami-v2", nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return map[string]any{"state": "unreachable", "message": "Could not reach Hugging Face to check the token."}
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return map[string]any{"state": "invalid"}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return map[string]any{"state": "unreachable", "message": "Hugging Face answered " + strconv.Itoa(resp.StatusCode) + " while checking the token."}
	}
	var who struct {
		Name *string `json:"name"`
		Auth *struct {
			AccessToken *struct {
				Role *string `json:"role"`
			} `json:"accessToken"`
		} `json:"auth"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&who) != nil {
		return map[string]any{"state": "unreachable", "message": "Hugging Face sent an unexpected answer while checking the token."}
	}
	var role any
	if who.Auth != nil && who.Auth.AccessToken != nil && who.Auth.AccessToken.Role != nil {
		role = *who.Auth.AccessToken.Role
	}
	var name any
	if who.Name != nil {
		name = *who.Name
	}
	return map[string]any{"state": "valid", "name": name, "role": role}
}

func (s *Server) hfTokenStatus(ctx context.Context) map[string]any {
	cfg, _ := s.Store.LoadConfig(ctx)
	saved := cfg != nil && cfg.HFToken != ""
	env := os.Getenv("HF_TOKEN") != ""
	var source any
	switch {
	case saved:
		source = "saved"
	case env:
		source = "env"
	}
	return map[string]any{"configured": saved || env, "source": source, "envAvailable": env}
}

func (s *Server) saveHFToken(ctx context.Context, token string) error {
	return rootpkg.WithOperation(s.Root.Dir, "update runtime configuration", func() error { return s.saveHFTokenLocked(ctx, token) })
}

func (s *Server) saveHFTokenLocked(ctx context.Context, token string) error {
	cfg, err := s.Store.LoadConfig(ctx)
	if err != nil {
		return err
	}
	cfg.HFToken = token
	return s.Store.SaveConfig(ctx, cfg)
}

func (s *Server) hfToken(w http.ResponseWriter, r *http.Request) {
	p := s.principal(r)
	if !p.Authenticated() {
		unauthorized(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		methodNotAllowed(w, r, "GET, POST")
		return
	}
	if !p.Can("models:manage") {
		forbidden(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	ctx := r.Context()
	if r.Method == http.MethodGet {
		writeJSON(w, 200, s.hfTokenStatus(ctx))
		return
	}
	if r.ContentLength > 1024 {
		w.Header().Set("Connection", "close")
		writeError(w, r, 413, "invalid_request_error", "validation_failed", "Hugging Face token body exceeds the 1024 byte limit.", nil)
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 1025))
	if err != nil || len(b) > 1024 {
		w.Header().Set("Connection", "close")
		writeError(w, r, 413, "invalid_request_error", "validation_failed", "Hugging Face token body exceeds the 1024 byte limit.", nil)
		return
	}
	var in struct {
		Action   string  `json:"action"`
		Token    *string `json:"token"`
		Validate *bool   `json:"validate"`
	}
	if !json.Valid(b) {
		badRequest(w, r, "Invalid Hugging Face token payload.", nil)
		return
	}
	token := ""
	ok := strictDecode(b, &in)
	if in.Token != nil {
		token = strings.TrimSpace(*in.Token)
	}
	switch in.Action {
	case "save":
		ok = ok && in.Token != nil && hfTokenRe.MatchString(token) && !nullField(b, "validate")
	case "clear", "validate":
		ok = ok && in.Token == nil && in.Validate == nil
	default:
		ok = false
	}
	if !ok {
		badRequest(w, r, "Expected a Hugging Face access token without spaces (8 to 256 characters).", nil)
		return
	}
	switch in.Action {
	case "clear":
		if err := s.saveHFToken(ctx, ""); err != nil {
			serviceUnavailable(w, r, "Configuration")
			return
		}
		writeJSON(w, 200, s.hfTokenStatus(ctx))
	case "validate":
		cfg, _ := s.Store.LoadConfig(ctx)
		t := os.Getenv("HF_TOKEN")
		if cfg != nil && cfg.HFToken != "" {
			t = cfg.HFToken
		}
		if t == "" {
			writeError(w, r, 409, "invalid_request_error", "hf_token_missing", "No Hugging Face token is saved or set in HF_TOKEN.", nil)
			return
		}
		out := s.hfTokenStatus(ctx)
		out["check"] = CheckHFToken(ctx, t)
		writeJSON(w, 200, out)
	case "save":
		var check map[string]any
		if in.Validate == nil || *in.Validate {
			check = CheckHFToken(ctx, token)
			if check["state"] == "invalid" {
				writeError(w, r, 400, "invalid_request_error", "hf_token_invalid",
					"Hugging Face rejected that token, so it was not saved. Copy a fresh read token from huggingface.co/settings/tokens.", nil)
				return
			}
		}
		if err := s.saveHFToken(ctx, token); err != nil {
			serviceUnavailable(w, r, "Configuration")
			return
		}
		out := s.hfTokenStatus(ctx)
		if check != nil {
			out["check"] = check
		}
		writeJSON(w, 200, out)
	}
}

// nullField reports a JSON object member that is present and null (zod's
// optional() rejects null).
func nullField(b []byte, name string) bool {
	var m map[string]json.RawMessage
	json.Unmarshal(b, &m)
	v, ok := m[name]
	return ok && string(v) == "null"
}

// onlyFields reports a JSON object whose members other than "action" are all
// in fields and none is null (zod's strict, non-nullable objects).
func onlyFields(b []byte, fields ...string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return false
	}
	for k, v := range m {
		if k != "action" && (!contains(fields, k) || string(v) == "null") {
			return false
		}
	}
	return true
}
