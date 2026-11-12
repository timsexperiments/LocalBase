package keys

import (
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
	"sync"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
	"github.com/timsexperiments/LocalBase/go/internal/lease"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Admin APIs (management-http.ts): /_localbase/access-management,
// /_localbase/api-keys, and /_localbase/hf-token.

// Handler serves the key and access APIs, and the Hugging Face token, for one gateway.
type Handler struct {
	opts Options
	// adminMu is held from the checks on a change to keys or access through its write:
	// each check reads state the other changes can alter.
	adminMu sync.Mutex
	// beforeAdminWrite runs after those checks, before the write (tests only).
	beforeAdminWrite func()
}

// Options are what Handler needs from the gateway.
type Options struct {
	Store *store.Store
	// RootDir holds the access policy and the Hugging Face token's lease.
	RootDir string
	// LLM is told a new Hugging Face token, for its downloads; nil when there is none.
	LLM *runtime.LLM
	// Browser is browser sign-in (nil: off). Saving access settings needs a restart, so it doesn't
	// change while the gateway runs.
	Browser *auth.Browser
	// Caller is who a request is from.
	Caller func(*http.Request) *auth.Principal
}

// NewHandler returns the key and access APIs for o.
func NewHandler(o Options) *Handler { return &Handler{opts: o} }

// errCallerGone is a key that stopped working after its request was authenticated.
var errCallerGone = errors.New("the calling key is no longer active")

// adminCaller reads what the caller holds now, with adminMu held. A key or a role can
// change between authenticating a request and acting on it, and the change that wins
// that race may be the one taking this caller's permissions away.
func (h *Handler) adminCaller(ctx context.Context, p *auth.Principal) (auth.KeyCaller, error) {
	switch {
	case p.Kind == "api-key":
		k, err := h.opts.Store.APIKeyByID(ctx, p.ID)
		var missing *store.ErrKeyNotFound
		if errors.As(err, &missing) || (err == nil && !auth.ActiveKey(*k)) {
			return auth.KeyCaller{}, errCallerGone
		}
		if err != nil {
			return auth.KeyCaller{}, err
		}
		return auth.KeyCaller{Holds: auth.Canonical(k.Scopes), Key: k}, nil
	case p.Kind == "browser-session" && p.Identity != nil && h.opts.Browser.Enabled():
		return auth.KeyCaller{Holds: h.opts.Browser.Authorize(ctx, p.Identity).Permissions}, nil
	}
	return auth.KeyCaller{Holds: p.Permissions}, nil
}

func (h *Handler) domainError(w http.ResponseWriter, err error) bool {
	var ae *auth.AccessError
	if !errors.As(err, &ae) {
		return false
	}
	mapped := map[string][2]any{
		"policy-not-configured":      {"policy_not_configured", 409},
		"managed-user-not-found":     {"managed_user_not_found", 404},
		"managed-user-exists":        {"managed_user_exists", 409},
		"role-not-found":             {"role_not_found", 400},
		"policy-conflict":            {"policy_conflict", 409},
		"policy-revision-conflict":   {"policy_revision_conflict", 409},
		"would-lock-out-admins":      {"would_lock_out_admins", 409},
		"exceeds-caller-permissions": {"exceeds_caller_permissions", 403},
		"exceeds-caller-expiry":      {"exceeds_caller_expiry", 403},
		"issuer-required":            {"issuer_required", 400},
		"issuer-unknown":             {"issuer_unknown", 400},
	}[ae.Code]
	switch ae.Code {
	case "would-lock-out-admins", "exceeds-caller-permissions", "exceeds-caller-expiry", "issuer-required", "issuer-unknown":
		// Rules of this gateway: the error carries its own explanation.
		w.Header().Set("Cache-Control", "no-store")
		httpx.JSON(w, mapped[1].(int), map[string]any{"error": map[string]string{"code": mapped[0].(string), "message": ae.Message}})
		return true
	}
	httpx.ManagementError(w, mapped[1].(int), mapped[0].(string))
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
		if !slices.Contains(auth.Permissions, sc) {
			return nil, false
		}
	}
	return auth.Canonical(scopes), true
}

func (h *Handler) APIKeys(w http.ResponseWriter, r *http.Request) {
	p := h.opts.Caller(r)
	if !p.Authenticated() {
		if httpx.CredentialLookupFailed(w, r) {
			return
		}
		httpx.ManagementError(w, 401, "invalid_api_key")
		return
	}
	ctx := r.Context()
	if r.Method == http.MethodGet {
		if !p.Can("keys:read") {
			httpx.ManagementError(w, 403, "insufficient_permissions")
			return
		}
		keys, err := h.opts.Store.ListAPIKeys(ctx)
		if err != nil {
			httpx.ServiceUnavailable(w, r, "Key management")
			return
		}
		out := []map[string]any{}
		for i := range keys {
			out = append(out, publicKey(&keys[i]))
		}
		httpx.NoStore(w, 200, map[string]any{"keys": out})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(405)
		return
	}
	if !p.Can("keys:manage") {
		httpx.ManagementError(w, 403, "insufficient_permissions")
		return
	}
	body, ok := httpx.ReadManagement(w, r)
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
	if !httpx.StrictDecode(body, &in) {
		httpx.ManagementError(w, 400, "validation_failed")
		return
	}
	keyID := ""
	if in.KeyID != nil {
		keyID = *in.KeyID
	}
	var valid bool
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
	valid = valid && httpx.OnlyFields(body, map[string][]string{"create": {"name", "expiresDays", "scopes"}, "rotate": {"keyId"},
		"revoke": {"keyId"}, "set-scopes": {"keyId", "scopes"}}[in.Action]...)
	var scopes []string
	if valid && in.Scopes != nil {
		scopes, valid = validScopes(*in.Scopes)
	}
	if !valid {
		httpx.ManagementError(w, 400, "validation_failed")
		return
	}
	// From the checks to the write, nothing else changes keys or access.
	h.adminMu.Lock()
	defer h.adminMu.Unlock()
	caller, err := h.adminCaller(ctx, p)
	switch {
	case errors.Is(err, errCallerGone):
		httpx.ManagementError(w, 401, "invalid_api_key")
		return
	case err != nil:
		httpx.ServiceUnavailable(w, r, "Key management")
		return
	case !slices.Contains(caller.Holds, "keys:manage"):
		httpx.ManagementError(w, 403, "insufficient_permissions")
		return
	}
	var target *store.APIKey
	if in.Action != "create" {
		if target, err = h.opts.Store.APIKeyByID(ctx, keyID); err != nil {
			if missing := (*store.ErrKeyNotFound)(nil); errors.As(err, &missing) {
				httpx.ManagementError(w, 404, "key_not_found")
			} else {
				httpx.ServiceUnavailable(w, r, "Key management") // a key that can't be read may still be there
			}
			return
		}
	}
	days := 0
	if in.ExpiresDays != nil {
		days = int(*in.ExpiresDays)
	}
	switch in.Action {
	case "create":
		err = caller.CheckCreate(scopes, days)
	case "rotate":
		err = caller.CheckRotate(target)
	case "revoke":
		err = caller.CheckRevoke(target)
	case "set-scopes":
		err = caller.CheckSetScopes(target, scopes)
	}
	if err != nil {
		h.domainError(w, err)
		return
	}
	// Lockout rules (DESIGN §13.6): never the last principal with access:manage.
	if in.Action != "create" {
		cfg, err := auth.LoadAccess(h.opts.RootDir)
		if err != nil {
			httpx.ServiceUnavailable(w, r, "Key management")
			return
		}
		g, err := auth.LoadGuard(ctx, h.opts.Store, cfg)
		if err != nil {
			httpx.ServiceUnavailable(w, r, "Key management")
			return
		}
		if err := g.CheckKeyChange(caller.KeyID(), target, in.Action, scopes); err != nil {
			h.domainError(w, err)
			return
		}
	}
	if h.beforeAdminWrite != nil {
		h.beforeAdminWrite()
	}
	switch in.Action {
	case "create":
		k, raw, err := h.opts.Store.CreateAPIKeyExpiring(ctx, *in.Name, scopes, days)
		if err != nil {
			httpx.ServiceUnavailable(w, r, "Key management")
			return
		}
		httpx.NoStore(w, 201, map[string]any{"key": publicKey(k), "secret": raw})
	case "rotate":
		k, raw, err := h.opts.Store.RotateAPIKey(ctx, keyID)
		if err != nil {
			httpx.ServiceUnavailable(w, r, "Key management")
			return
		}
		httpx.NoStore(w, 200, map[string]any{"key": publicKey(k), "secret": raw})
	case "revoke":
		k, err := h.opts.Store.RevokeAPIKey(ctx, keyID)
		if err != nil {
			httpx.ServiceUnavailable(w, r, "Key management")
			return
		}
		httpx.NoStore(w, 200, map[string]any{"key": publicKey(k)})
	case "set-scopes":
		k, err := h.opts.Store.SetAPIKeyScopes(ctx, keyID, scopes)
		if err != nil {
			httpx.ServiceUnavailable(w, r, "Key management")
			return
		}
		httpx.NoStore(w, 200, map[string]any{"key": publicKey(k)})
	}
}

// --- access management ------------------------------------------------------------------

func (h *Handler) AccessManagement(w http.ResponseWriter, r *http.Request) {
	p := h.opts.Caller(r)
	if !p.Authenticated() {
		if httpx.CredentialLookupFailed(w, r) {
			return
		}
		httpx.ManagementError(w, 401, "invalid_api_key")
		return
	}
	ctx := r.Context()
	root := h.opts.RootDir
	if r.Method == http.MethodGet {
		if !p.Can("access:read") {
			httpx.ManagementError(w, 403, "insufficient_permissions")
			return
		}
		cfg, err := auth.LoadAccess(root)
		policy, perr := auth.LoadPolicy(ctx, h.opts.Store.DB)
		users, uerr := auth.ListUsers(ctx, h.opts.Store)
		email, eerr := auth.LoadEmail(root)
		if err != nil || perr != nil || uerr != nil || eerr != nil {
			httpx.ServiceUnavailable(w, r, "Access management")
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
		out := map[string]any{"config": summary, "policy": policyOut, "policyRevision": auth.Revision(policy),
			"users": users, "roles": roles, "emailDelivery": emailSummary}
		if open := auth.OpenSignIn(cfg, policy); open != "" {
			out["openSignIn"] = open
		}
		signedIn, err := auth.SignedIn(ctx, h.opts.Store)
		if err != nil {
			httpx.ServiceUnavailable(w, r, "Access management")
			return
		}
		if len(signedIn) > 0 {
			out["signedIn"] = signedIn
		}
		if len(cfg.Issuers()) > 0 {
			left, err := auth.CountUnbound(ctx, h.opts.Store.DB)
			if err != nil {
				httpx.ServiceUnavailable(w, r, "Access management")
				return
			}
			if left.Total() > 0 {
				out["unboundBindings"] = left.Total()
			}
		}
		httpx.NoStore(w, 200, out)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(405)
		return
	}
	if !p.Can("access:read") && !p.Can("access:manage") {
		httpx.ManagementError(w, 403, "insufficient_permissions")
		return
	}
	body, ok := httpx.ReadManagement(w, r)
	if !ok {
		return
	}
	// Which permission this needs. Anything but a clear test-policy needs the stronger one.
	var head struct{ Action string }
	required := "access:manage"
	if json.Unmarshal(body, &head) == nil && head.Action == "test-policy" {
		required = "access:read"
	}
	in, valid := parseAccessRequest(body)
	if !valid {
		// The one refusal Admin must explain (DESIGN §13.6 item 12).
		if in != nil && in.lockedOut {
			httpx.NoStore(w, 400, map[string]any{"error": map[string]string{"code": "validation_failed", "message": auth.PolicyLockoutMessage}})
			return
		}
		httpx.ManagementError(w, 400, "validation_failed")
		return
	}
	if !p.Can(required) {
		httpx.ManagementError(w, 403, "insufficient_permissions")
		return
	}
	if in.Action != "test-policy" && in.Action != "test-email-delivery" {
		op := "update browser access"
		if in.Action == "invite-user" {
			op = "invite browser user"
		}
		lease, err := lease.AcquireOperation(root, op)
		if err != nil {
			httpx.ServiceUnavailable(w, r, "Access management")
			return
		}
		defer lease.Release()
		// From the checks to the write, nothing else changes keys or access.
		h.adminMu.Lock()
		defer h.adminMu.Unlock()
	}
	current, err := auth.LoadAccess(root)
	if err != nil {
		httpx.ServiceUnavailable(w, r, "Access management")
		return
	}
	if in.Action != "test-policy" && in.Action != "test-email-delivery" {
		caller, err := h.adminCaller(ctx, p)
		switch {
		case errors.Is(err, errCallerGone):
			httpx.ManagementError(w, 401, "invalid_api_key")
			return
		case err != nil:
			httpx.ServiceUnavailable(w, r, "Access management")
			return
		case !slices.Contains(caller.Holds, "access:manage"):
			httpx.ManagementError(w, 403, "insufficient_permissions")
			return
		}
		g, err := auth.LoadGuard(ctx, h.opts.Store, current)
		if err != nil {
			httpx.ServiceUnavailable(w, r, "Access management")
			return
		}
		if err := prepareAccessChange(g, caller.Holds, p.Identity, in); err != nil {
			h.domainError(w, err)
			return
		}
	}
	// lockout refuses a change that would lock the caller or everyone out of
	// access management (a Go addition; see auth.Guard). It reports whether
	// it wrote a response.
	lockout := func(email string, self bool, change func(*auth.Guard)) bool {
		g, err := auth.LoadGuard(ctx, h.opts.Store, current)
		if err != nil {
			httpx.ServiceUnavailable(w, r, "Access management")
			return true
		}
		var caller *auth.Identity
		if p.Kind == "browser-session" {
			caller = p.Identity
		}
		if self && g.IsCaller(caller, email) {
			h.domainError(w, auth.ErrSelfRecord)
			return true
		}
		if err := g.CheckChange(caller, change); err != nil {
			h.domainError(w, err)
			return true
		}
		if h.beforeAdminWrite != nil {
			h.beforeAdminWrite()
		}
		return false
	}
	// signInLockout is lockout for a change of who can sign in: cfg is the configuration
	// it would leave, or nil for none.
	signInLockout := func(cfg *auth.BrowserAccess) bool {
		return lockout("", false, func(next *auth.Guard) {
			next.ProviderPerms, next.Issuers = nil, cfg.Issuers()
			if cfg != nil {
				next.ProviderPerms = cfg.Permissions
			}
		})
	}
	configured := func(cfg *auth.BrowserAccess) {
		httpx.NoStore(w, 200, map[string]any{"config": cfg.Summary(), "restartRequired": true})
	}
	save := func(cfg *auth.BrowserAccess) bool {
		if err := auth.SaveAccess(root, cfg); err != nil {
			httpx.ManagementError(w, 400, "validation_failed")
			return false
		}
		return true
	}
	switch in.Action {
	case "test-policy":
		if current == nil {
			httpx.ManagementError(w, 409, "provider_not_configured")
			return
		}
		policy, err := auth.LoadPolicy(ctx, h.opts.Store.DB)
		if err != nil {
			httpx.ServiceUnavailable(w, r, "Access management")
			return
		}
		d, err := auth.ResolvePolicy(ctx, h.opts.Store, *in.Identity, false)
		if err != nil {
			httpx.ServiceUnavailable(w, r, "Access management")
			return
		}
		if d == nil {
			d = &auth.Decision{MatchedRoles: []string{}, Permissions: current.Permissions}
		}
		httpx.NoStore(w, 200, map[string]any{"policyConfigured": policy != nil, "matchedRoles": d.MatchedRoles, "permissions": d.Permissions})
	case "test-email-delivery":
		email, _ := auth.LoadEmail(root)
		if email == nil {
			httpx.ManagementError(w, 409, "email_delivery_not_configured")
			return
		}
		if err := auth.SendEmail(email, in.To, "LocalBase email delivery test", "LocalBase successfully sent this test email."); err != nil {
			httpx.ManagementError(w, 502, "email_delivery_failed")
			return
		}
		httpx.NoStore(w, 200, map[string]any{"emailDelivered": true, "to": in.To})
	case "invite-user":
		if current == nil {
			httpx.ManagementError(w, 409, "provider_not_configured")
			return
		}
		var email *auth.EmailConfig
		if in.SendEmail {
			if email, _ = auth.LoadEmail(root); email == nil {
				httpx.ManagementError(w, 409, "email_delivery_not_configured")
				return
			}
		}
		user, err := auth.InviteUser(ctx, h.opts.Store, in.Email, in.Roles, in.Issuer)
		if err != nil {
			if !h.domainError(w, err) {
				httpx.ManagementError(w, 400, "validation_failed")
			}
			return
		}
		signIn := current.Origin + "/app/login"
		delivered := false
		if email != nil {
			delivered = auth.SendEmail(email, user.Email, "You were invited to LocalBase",
				"You were invited to LocalBase. Sign in here:\n\n"+signIn) == nil
		}
		httpx.NoStore(w, 201, map[string]any{"user": user, "signInUrl": signIn, "emailDelivered": delivered})
	case "configure-cloudflare":
		cfg := &auth.BrowserAccess{Provider: *in.Provider, Origin: in.Origin, Permissions: in.Permissions}
		changed := auth.AccessChanged(current, cfg)
		if !signInLockout(cfg) && save(cfg) {
			if !changed {
				httpx.Unchanged(r)
			}
			configured(cfg)
		}
	case "upsert-oidc", "upsert-github", "upsert-magic-link":
		if in.Action == "upsert-magic-link" {
			if email, _ := auth.LoadEmail(root); email == nil {
				httpx.ManagementError(w, 409, "email_delivery_not_configured")
				return
			}
			if current != nil && current.Provider.Kind == "direct" {
				for _, reg := range current.Provider.Registrations {
					if reg.Kind == "magic-link" && reg.ID != in.Registration.ID {
						httpx.ManagementError(w, 400, "validation_failed")
						return
					}
				}
			}
		}
		cfg := auth.UpsertRegistration(current, *in.Registration, in.Origin, in.Permissions)
		changed := auth.AccessChanged(current, cfg)
		if !signInLockout(cfg) && save(cfg) {
			if !changed {
				httpx.Unchanged(r)
			}
			configured(cfg)
		}
	case "remove-registration":
		kind, cfg := auth.RemoveRegistration(current, in.RegistrationID)
		switch kind {
		case "not-found":
			httpx.ManagementError(w, 404, "registration_not_found")
			return
		case "disabled":
			if signInLockout(nil) {
				return
			}
			if _, err := auth.DisableAccess(root); err != nil {
				httpx.ServiceUnavailable(w, r, "Access management")
				return
			}
			httpx.NoStore(w, 200, map[string]any{"removedRegistrationId": in.RegistrationID, "config": nil, "restartRequired": true})
		default:
			if !signInLockout(cfg) && save(cfg) {
				httpx.NoStore(w, 200, map[string]any{"removedRegistrationId": in.RegistrationID, "config": cfg.Summary(), "restartRequired": true})
			}
		}
	case "disable":
		if signInLockout(nil) {
			return
		}
		disabled, err := auth.DisableAccess(root)
		if err != nil {
			httpx.ServiceUnavailable(w, r, "Access management")
			return
		}
		if !disabled {
			httpx.Unchanged(r)
		}
		httpx.NoStore(w, 200, map[string]any{"disabled": disabled, "restartRequired": disabled})
	case "configure-email-delivery":
		// A configuration that can't be read counts as changed: the save replaces it.
		previous, perr := auth.LoadEmail(root)
		if err := auth.SaveEmail(root, in.Config); err != nil {
			httpx.ManagementError(w, 400, "validation_failed")
			return
		}
		if perr == nil && !auth.EmailChanged(previous, in.Config) {
			httpx.Unchanged(r)
		}
		httpx.NoStore(w, 200, map[string]any{"emailDelivery": in.Config.Summary(), "restartRequired": false})
	case "disable-email-delivery":
		if current != nil && current.Provider.Kind == "direct" {
			for _, reg := range current.Provider.Registrations {
				if reg.Kind == "magic-link" {
					httpx.ManagementError(w, 409, "email_delivery_in_use")
					return
				}
			}
		}
		disabled, err := auth.DisableEmail(root)
		if err != nil {
			httpx.ServiceUnavailable(w, r, "Access management")
			return
		}
		if !disabled {
			httpx.Unchanged(r)
		}
		httpx.NoStore(w, 200, map[string]any{"emailDeliveryDisabled": disabled, "restartRequired": false})
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
				// The record stays, and a sign-in stays refused for it: bindings don't
				// reach a removed person until they are invited again.
				u.Status, u.Roles = "removed", nil
			}
		}) {
			return
		}
		switch in.Action {
		case "replace-user-roles":
			user, err = auth.ReplaceUserRoles(ctx, h.opts.Store, in.Email, in.Roles)
		case "enable-user":
			user, err = auth.SetUserStatus(ctx, h.opts.Store, in.Email, "active")
		case "disable-user":
			user, err = auth.SetUserStatus(ctx, h.opts.Store, in.Email, "disabled")
		default:
			user, err = auth.RemoveUser(ctx, h.opts.Store, in.Email)
		}
		if err != nil {
			if !h.domainError(w, err) {
				httpx.ManagementError(w, 400, "validation_failed")
			}
			return
		}
		httpx.NoStore(w, 200, map[string]any{"user": user})
	case "apply-policy", "clear-policy":
		if in.Action == "apply-policy" && current == nil {
			httpx.ManagementError(w, 409, "provider_not_configured")
			return
		}
		existing, err := auth.LoadPolicy(ctx, h.opts.Store.DB)
		if err != nil {
			httpx.ServiceUnavailable(w, r, "Access management")
			return
		}
		rev := auth.Revision(existing)
		if (rev == nil) != (in.ExpectedRevision == nil) || (rev != nil && *rev != *in.ExpectedRevision) {
			httpx.ManagementError(w, 409, "policy_revision_conflict")
			return
		}
		if lockout("", false, func(next *auth.Guard) {
			if in.Action == "clear-policy" {
				// Managed users block clearing (TS refuses with policy_conflict);
				// leave that refusal to ClearPolicy.
				if next.Managed() == 0 {
					next.Policy = nil
				}
				return
			}
			// Judge the policy as it will be stored (normalized); an invalid
			// one is left for ApplyPolicy to reject.
			if in.Policy == nil {
				return
			}
			if candidate := in.Policy.Clone(); candidate.Normalize() == nil {
				next.Policy = candidate
			}
		}) {
			return
		}
		if in.Action == "clear-policy" {
			cleared, err := auth.ClearPolicy(ctx, h.opts.Store)
			if err != nil {
				if !h.domainError(w, err) {
					httpx.ServiceUnavailable(w, r, "Access management")
				}
				return
			}
			if !cleared {
				httpx.Unchanged(r)
			}
			httpx.NoStore(w, 200, map[string]any{"cleared": cleared, "policyRevision": nil, "restartRequired": false})
			return
		}
		if err := auth.ApplyPolicy(ctx, h.opts.Store, in.Policy); err != nil {
			if !h.domainError(w, err) {
				httpx.ManagementError(w, 400, "validation_failed")
			}
			return
		}
		applied := auth.Revision(in.Policy)
		if rev != nil && *rev == *applied { // the revision is the policy's content, in any order
			httpx.Unchanged(r)
		}
		httpx.NoStore(w, 200, map[string]any{"policy": in.Policy, "policyRevision": applied, "restartRequired": false})
	}
}

// prepareAccessChange names the provider on what needs one (DESIGN §13.6, item 5b), and
// refuses a change that reaches past what the caller holds (item 5a).
func prepareAccessChange(g *auth.Guard, holds []string, identity *auth.Identity, in *accessRequest) error {
	switch in.Action {
	case "configure-cloudflare", "upsert-oidc", "upsert-github", "upsert-magic-link":
		return g.CheckSignInChange(holds, in.Permissions)
	case "remove-registration", "disable", "configure-email-delivery", "disable-email-delivery":
		return g.CheckSignInChange(holds, nil)
	case "invite-user":
		issuer, err := auth.InviteIssuer(in.Issuer, in.Email, g.Issuers, identity)
		if err != nil {
			return err
		}
		in.Issuer = issuer
		return g.CheckGrantRoles(holds, in.Roles)
	case "replace-user-roles":
		if err := g.CheckUser(holds, in.Email); err != nil {
			return err
		}
		return g.CheckGrantRoles(holds, in.Roles)
	case "enable-user", "disable-user", "remove-user":
		return g.CheckUser(holds, in.Email)
	case "apply-policy":
		if err := in.Policy.BindIssuers(g.Issuers, identity); err != nil {
			return err
		}
		// With no provider at all, the answer is that one is needed first.
		if len(g.Issuers) > 0 {
			if err := g.CheckBindingIssuers(in.Policy); err != nil {
				return err
			}
		}
		return g.CheckPolicyChange(holds, in.Policy)
	case "clear-policy":
		return g.CheckPolicyChange(holds, nil)
	}
	return nil
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
	Issuer           string             `json:"issuer"`
	Policy           *auth.Policy       `json:"policy"`
	ExpectedRevision *string            `json:"expectedPolicyRevision"`
	Identity         *auth.Identity     `json:"identity"`
	raw              map[string]json.RawMessage
	// lockedOut: the policy failed only because no one could manage access.
	lockedOut bool
}

var revisionRe = regexp.MustCompile(`^[a-f0-9]{64}$`)

// parseAccessRequest applies the per-action TS schema: exactly the action's
// fields, each valid.
func parseAccessRequest(b []byte) (*accessRequest, bool) {
	var in accessRequest
	if !httpx.StrictDecode(b, &in) {
		return nil, false
	}
	_ = json.Unmarshal(b, &in.raw) // strictDecode has just accepted b as this object.
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
		"invite-user":              {"email", "roles", "sendEmail", "issuer"},
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
		if k != "action" && !slices.Contains(fields, k) {
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
		// The policy itself is normalized, not a copy: what follows (the issuer binding, its
		// refusals' words, the lockout check) judges it as it will be stored.
		if err := in.Policy.Normalize(); err != nil {
			if errors.Is(err, auth.ErrPolicyLockout) {
				return &accessRequest{lockedOut: true}, false
			}
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

func (h *Handler) hfTokenStatus(ctx context.Context) map[string]any {
	cfg, _ := h.opts.Store.LoadConfig(ctx)
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

func (h *Handler) saveHFToken(ctx context.Context, token string) error {
	return lease.WithOperation(h.opts.RootDir, "update runtime configuration", func() error { return h.saveHFTokenLocked(ctx, token) })
}

func (h *Handler) saveHFTokenLocked(ctx context.Context, token string) error {
	cfg, err := h.opts.Store.LoadConfig(ctx)
	if err != nil {
		return err
	}
	cfg.HFToken = token
	if err := h.opts.Store.SaveConfig(ctx, cfg); err != nil {
		return err
	}
	if h.opts.LLM != nil { // Downloads read it from here. Only this changes: the rest may hold overrides.
		next := h.opts.LLM.Config().Clone()
		next.HFToken = token
		h.opts.LLM.SetConfig(next)
	}
	return nil
}

func (h *Handler) HFToken(w http.ResponseWriter, r *http.Request) {
	p := h.opts.Caller(r)
	if !p.Authenticated() {
		httpx.Unauthorized(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		// Checked here, not by the route table's gate: this handler authenticates the caller first, and 401 comes before 405.
		httpx.MethodNotAllowed(w, r, "GET, POST")
		return
	}
	if !p.Can("models:manage") {
		httpx.Forbidden(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	ctx := r.Context()
	if r.Method == http.MethodGet {
		httpx.JSON(w, 200, h.hfTokenStatus(ctx))
		return
	}
	if r.ContentLength > 1024 {
		w.Header().Set("Connection", "close")
		httpx.Error(w, r, 413, "invalid_request_error", "validation_failed", "Hugging Face token body exceeds the 1024 byte limit.", nil)
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 1025))
	if err != nil || len(b) > 1024 {
		w.Header().Set("Connection", "close")
		httpx.Error(w, r, 413, "invalid_request_error", "validation_failed", "Hugging Face token body exceeds the 1024 byte limit.", nil)
		return
	}
	var in struct {
		Action   string  `json:"action"`
		Token    *string `json:"token"`
		Validate *bool   `json:"validate"`
	}
	if !json.Valid(b) {
		httpx.BadRequest(w, r, "Invalid Hugging Face token payload.", nil)
		return
	}
	token := ""
	ok := httpx.StrictDecode(b, &in)
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
		httpx.BadRequest(w, r, "Expected a Hugging Face access token without spaces (8 to 256 characters).", nil)
		return
	}
	switch in.Action {
	case "clear":
		if err := h.saveHFToken(ctx, ""); err != nil {
			httpx.ServiceUnavailable(w, r, "Configuration")
			return
		}
		httpx.JSON(w, 200, h.hfTokenStatus(ctx))
	case "validate":
		cfg, _ := h.opts.Store.LoadConfig(ctx)
		t := os.Getenv("HF_TOKEN")
		if cfg != nil && cfg.HFToken != "" {
			t = cfg.HFToken
		}
		if t == "" {
			httpx.Error(w, r, 409, "invalid_request_error", "hf_token_missing", "No Hugging Face token is saved or set in HF_TOKEN.", nil)
			return
		}
		out := h.hfTokenStatus(ctx)
		out["check"] = CheckHFToken(ctx, t)
		httpx.JSON(w, 200, out)
	case "save":
		var check map[string]any
		if in.Validate == nil || *in.Validate {
			check = CheckHFToken(ctx, token)
			if check["state"] == "invalid" {
				httpx.Error(w, r, 400, "invalid_request_error", "hf_token_invalid",
					"Hugging Face rejected that token, so it was not saved. Copy a fresh read token from huggingface.co/settings/tokens.", nil)
				return
			}
		}
		if err := h.saveHFToken(ctx, token); err != nil {
			httpx.ServiceUnavailable(w, r, "Configuration")
			return
		}
		out := h.hfTokenStatus(ctx)
		if check != nil {
			out["check"] = check
		}
		httpx.JSON(w, 200, out)
	}
}

// nullField reports a JSON object member that is present and null (zod's
// optional() rejects null).
func nullField(b []byte, name string) bool {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(b, &m) // Not an object has no such member.
	v, ok := m[name]
	return ok && string(v) == "null"
}
