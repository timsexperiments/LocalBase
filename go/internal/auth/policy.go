package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// Browser access policy (access-control.ts): roles with permissions,
// bindings by subject, email or email domain, and an optional default role.

// Role is a named permission set.
type Role struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Permissions []string `json:"permissions"`
}

// Binding grants a role to a subject, an email or an email domain. Issuer is the sign-in
// provider it trusts: a subject belongs to one, and an email or domain binding names the
// one that vouches for the address. An email or domain binding without one matches nobody.
type Binding struct {
	Kind    string `json:"kind"` // subject | email | email-domain
	Role    string `json:"role"`
	Issuer  string `json:"issuer,omitempty"`
	Subject string `json:"subject,omitempty"`
	Email   string `json:"email,omitempty"`
	Domain  string `json:"domain,omitempty"`
}

// MarshalJSON writes the fields of the binding's kind in TS key order.
func (b Binding) MarshalJSON() ([]byte, error) {
	switch b.Kind {
	case "subject":
		return json.Marshal(struct {
			Kind    string `json:"kind"`
			Role    string `json:"role"`
			Issuer  string `json:"issuer"`
			Subject string `json:"subject"`
		}{b.Kind, b.Role, b.Issuer, b.Subject})
	case "email":
		return json.Marshal(struct {
			Kind   string `json:"kind"`
			Role   string `json:"role"`
			Email  string `json:"email"`
			Issuer string `json:"issuer,omitempty"`
		}{b.Kind, b.Role, b.Email, b.Issuer})
	}
	return json.Marshal(struct {
		Kind   string `json:"kind"`
		Role   string `json:"role"`
		Domain string `json:"domain"`
		Issuer string `json:"issuer,omitempty"`
	}{b.Kind, b.Role, b.Domain, b.Issuer})
}

// Policy is the access control configuration.
type Policy struct {
	Roles       []Role    `json:"roles"`
	Bindings    []Binding `json:"bindings"`
	DefaultRole *string   `json:"defaultRole"`
}

// StarterPolicy gives one address every permission, through one provider, and nobody else
// anything. It has a member role for people added later, bound to nobody.
func StarterPolicy(email, issuer string) *Policy {
	return &Policy{
		Roles: []Role{
			{Name: "admin", Description: "Full LocalBase administration", Permissions: slices.Clone(Permissions)},
			{Name: "member", Description: "Chat and model discovery", Permissions: []string{"inference:chat", "models:read"}},
		},
		Bindings: []Binding{{Kind: "email", Role: "admin", Email: email, Issuer: issuer}},
	}
}

// AccessError is a domain error with a TS code.
type AccessError struct{ Code, Message string }

func (e *AccessError) Error() string { return e.Message }

var (
	roleNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	domainRe   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$`)
)

// ValidEmail is a lowercased address or "".
func ValidEmail(s string) string {
	if len(s) > 320 {
		return ""
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || !strings.Contains(s, "@") {
		return ""
	}
	return strings.ToLower(s)
}

func validBoundIssuer(issuer string) bool {
	return issuer == "" || (len(issuer) <= 2048 && validIssuer(issuer))
}

// Normalize validates a policy and normalizes emails, domains and
// permission order.
func (p *Policy) Normalize() error {
	if len(p.Roles) < 1 || len(p.Roles) > 64 {
		return errors.New("roles: must list 1 to 64 roles")
	}
	if len(p.Bindings) > 256 {
		return errors.New("bindings: at most 256")
	}
	roles := map[string]Role{}
	for i, r := range p.Roles {
		if len(r.Name) > 64 || !roleNameRe.MatchString(r.Name) || len(r.Description) > 256 {
			return fmt.Errorf("roles.%d: invalid role", i)
		}
		for _, perm := range r.Permissions {
			if !contains(Permissions, perm) {
				return fmt.Errorf("roles.%d.permissions: unknown permission %s", i, perm)
			}
		}
		p.Roles[i].Permissions = Canonical(r.Permissions)
		if _, dup := roles[r.Name]; dup {
			return errors.New("Role names must be unique.")
		}
		roles[r.Name] = p.Roles[i]
	}
	if p.DefaultRole != nil {
		if _, ok := roles[*p.DefaultRole]; !ok {
			return errors.New("Default role must exist.")
		}
	}
	seen := map[string]bool{}
	reachable := map[string]bool{}
	if p.DefaultRole != nil {
		reachable[*p.DefaultRole] = true
	}
	for i := range p.Bindings {
		b := &p.Bindings[i]
		if _, ok := roles[b.Role]; !ok {
			return fmt.Errorf("Unknown role: %s", b.Role)
		}
		switch b.Kind {
		case "subject":
			if b.Issuer == "" || len(b.Issuer) > 2048 || b.Subject == "" || len(b.Subject) > 512 || b.Email != "" || b.Domain != "" {
				return fmt.Errorf("bindings.%d: invalid subject binding", i)
			}
		case "email":
			if b.Email = ValidEmail(b.Email); b.Email == "" || b.Domain != "" || !validBoundIssuer(b.Issuer) {
				return fmt.Errorf("bindings.%d: invalid email binding", i)
			}
		case "email-domain":
			b.Domain = strings.ToLower(b.Domain)
			if !domainRe.MatchString(b.Domain) || len(b.Domain) > 253 || b.Email != "" || !validBoundIssuer(b.Issuer) {
				return fmt.Errorf("bindings.%d: invalid domain binding", i)
			}
		default:
			return fmt.Errorf("bindings.%d.kind: invalid", i)
		}
		key, _ := json.Marshal(b)
		if seen[string(key)] {
			return errors.New("Bindings must be unique.")
		}
		seen[string(key)] = true
		reachable[b.Role] = true
	}
	for name := range reachable {
		if contains(roles[name].Permissions, "access:manage") {
			if p.Bindings == nil {
				p.Bindings = []Binding{}
			}
			return nil
		}
	}
	return ErrPolicyLockout
}

// PolicyLockoutMessage refuses a policy no one could manage access under. It is
// plainer than TS's "At least one reachable role must grant access:manage."
// (DESIGN §13.6 item 12).
const PolicyLockoutMessage = "This policy would lock everyone out: at least one person it names must be able to manage access."

// ErrPolicyLockout is the refusal itself, so a caller can show why.
var ErrPolicyLockout = errors.New(PolicyLockoutMessage)

// Revision is the sha256 of the canonical policy (accessControlRevision).
func Revision(p *Policy) *string {
	if p == nil {
		return nil
	}
	roles := append([]Role{}, p.Roles...)
	sort.Slice(roles, func(i, j int) bool { return roles[i].Name < roles[j].Name })
	for i := range roles {
		perms := append([]string{}, roles[i].Permissions...)
		sort.Strings(perms)
		roles[i].Permissions = perms
	}
	bindings := append([]Binding{}, p.Bindings...)
	key := func(b Binding) string { s, _ := json.Marshal(b); return string(s) }
	sort.Slice(bindings, func(i, j int) bool { return key(bindings[i]) < key(bindings[j]) })
	if bindings == nil {
		bindings = []Binding{}
	}
	b, _ := json.Marshal(struct {
		Roles       []Role    `json:"roles"`
		Bindings    []Binding `json:"bindings"`
		DefaultRole *string   `json:"defaultRole"`
	}{roles, bindings, p.DefaultRole})
	sum := sha256.Sum256(b)
	s := hex.EncodeToString(sum[:])
	return &s
}

func bindingID(parts ...string) string {
	b, _ := json.Marshal(parts)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func now() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }
