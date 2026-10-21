package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/collate"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Browser access policy (access-control.ts): roles with permissions,
// bindings by subject, email or email domain, and an optional default role.

// Role is a named permission set.
type Role struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Permissions []string `json:"permissions"`
}

// Binding grants a role to a subject, an email or an email domain.
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
			Kind  string `json:"kind"`
			Role  string `json:"role"`
			Email string `json:"email"`
		}{b.Kind, b.Role, b.Email})
	}
	return json.Marshal(struct {
		Kind   string `json:"kind"`
		Role   string `json:"role"`
		Domain string `json:"domain"`
	}{b.Kind, b.Role, b.Domain})
}

// Policy is the access control configuration.
type Policy struct {
	Roles       []Role    `json:"roles"`
	Bindings    []Binding `json:"bindings"`
	DefaultRole *string   `json:"defaultRole"`
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
			if b.Email = ValidEmail(b.Email); b.Email == "" || b.Issuer != "" || b.Domain != "" {
				return fmt.Errorf("bindings.%d: invalid email binding", i)
			}
		case "email-domain":
			b.Domain = strings.ToLower(b.Domain)
			if !domainRe.MatchString(b.Domain) || len(b.Domain) > 253 || b.Issuer != "" || b.Email != "" {
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
	return errors.New("At least one reachable role must grant access:manage.")
}

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

// LoadPolicy returns the stored policy, or nil when none is configured.
func LoadPolicy(ctx context.Context, db *sql.DB) (*Policy, error) {
	var defaultID sql.NullString
	err := db.QueryRowContext(ctx, `SELECT default_role_id FROM auth_settings WHERE id = 'default'`).Scan(&defaultID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT id, name, description FROM auth_roles`)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	var roles []Role
	var ids []string
	for rows.Next() {
		var id string
		var r Role
		rows.Scan(&id, &r.Name, &r.Description)
		names[id] = r.Name
		ids = append(ids, id)
		roles = append(roles, r)
	}
	rows.Close()
	perms := map[string][]string{}
	if rows, err = db.QueryContext(ctx, `SELECT role_id, permission FROM auth_role_permissions`); err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, perm string
		rows.Scan(&id, &perm)
		perms[names[id]] = append(perms[names[id]], perm)
	}
	rows.Close()
	for i := range roles {
		roles[i].Permissions = Canonical(perms[roles[i].Name])
	}
	sort.Slice(roles, func(i, j int) bool { return localeLess(roles[i].Name, roles[j].Name) })
	p := &Policy{Roles: roles, Bindings: []Binding{}}
	scan := func(query string, build func(role string, a, b string) Binding) error {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var roleID, a, b string
			rows.Scan(&roleID, &a, &b)
			p.Bindings = append(p.Bindings, build(names[roleID], a, b))
		}
		return rows.Err()
	}
	if err := scan(`SELECT role_id, issuer, subject FROM auth_subject_role_bindings`, func(r, a, b string) Binding {
		return Binding{Kind: "subject", Role: r, Issuer: a, Subject: b}
	}); err != nil {
		return nil, err
	}
	if err := scan(`SELECT role_id, email, '' FROM auth_email_role_bindings`, func(r, a, _ string) Binding {
		return Binding{Kind: "email", Role: r, Email: a}
	}); err != nil {
		return nil, err
	}
	if err := scan(`SELECT role_id, domain, '' FROM auth_domain_role_bindings`, func(r, a, _ string) Binding {
		return Binding{Kind: "email-domain", Role: r, Domain: a}
	}); err != nil {
		return nil, err
	}
	key := func(b Binding) string { s, _ := json.Marshal(b); return string(s) }
	sort.Slice(p.Bindings, func(i, j int) bool { return localeLess(key(p.Bindings[i]), key(p.Bindings[j])) })
	if defaultID.Valid {
		if n, ok := names[defaultID.String]; ok {
			p.DefaultRole = &n
		}
	}
	return p, nil
}

// localeLess approximates String.localeCompare for ASCII names: case-
// insensitive first, punctuation before digits before letters.
func localeLess(a, b string) bool { return collate.Compare(a, b) < 0 }

// ApplyPolicy replaces the stored policy. Removing a role that a managed
// user holds is a policy-conflict.
func ApplyPolicy(ctx context.Context, st *store.Store, p *Policy) error {
	if err := p.Normalize(); err != nil {
		return err
	}
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ts := now()
	existing := map[string]string{}
	rows, err := tx.QueryContext(ctx, `SELECT id, name FROM auth_roles`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, name string
		rows.Scan(&id, &name)
		existing[name] = id
	}
	rows.Close()
	ids := map[string]string{}
	for _, r := range p.Roles {
		id, ok := existing[r.Name]
		if !ok {
			id = store.UUID()
		}
		ids[r.Name] = id
		if _, err := tx.ExecContext(ctx, `INSERT INTO auth_roles (id, name, description, created_at, updated_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (name) DO UPDATE SET description = excluded.description, updated_at = excluded.updated_at`,
			id, r.Name, r.Description, ts, ts); err != nil {
			return err
		}
	}
	for name, id := range existing {
		if _, kept := ids[name]; kept {
			continue
		}
		var held string
		err := tx.QueryRowContext(ctx, `SELECT r.name FROM auth_user_roles u JOIN auth_roles r ON u.role_id = r.id WHERE u.role_id = ? LIMIT 1`, id).Scan(&held)
		if err == nil {
			return &AccessError{"policy-conflict", "Cannot remove browser access role " + held + " while it is assigned to a managed user."}
		}
	}
	for _, q := range []string{`DELETE FROM auth_subject_role_bindings`, `DELETE FROM auth_email_role_bindings`,
		`DELETE FROM auth_domain_role_bindings`, `DELETE FROM auth_role_permissions`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	for name, id := range existing {
		if _, kept := ids[name]; !kept {
			if _, err := tx.ExecContext(ctx, `DELETE FROM auth_roles WHERE id = ?`, id); err != nil {
				return err
			}
		}
	}
	for _, r := range p.Roles {
		for _, perm := range r.Permissions {
			if _, err := tx.ExecContext(ctx, `INSERT INTO auth_role_permissions (role_id, permission) VALUES (?, ?)`, ids[r.Name], perm); err != nil {
				return err
			}
		}
	}
	for _, b := range p.Bindings {
		roleID := ids[b.Role]
		var err error
		switch b.Kind {
		case "subject":
			_, err = tx.ExecContext(ctx, `INSERT INTO auth_subject_role_bindings (id, role_id, issuer, subject) VALUES (?, ?, ?, ?)`,
				bindingID(b.Kind, roleID, b.Issuer, b.Subject), roleID, b.Issuer, b.Subject)
		case "email":
			_, err = tx.ExecContext(ctx, `INSERT INTO auth_email_role_bindings (id, role_id, email) VALUES (?, ?, ?)`,
				bindingID(b.Kind, roleID, b.Email), roleID, b.Email)
		case "email-domain":
			_, err = tx.ExecContext(ctx, `INSERT INTO auth_domain_role_bindings (id, role_id, domain) VALUES (?, ?, ?)`,
				bindingID(b.Kind, roleID, b.Domain), roleID, b.Domain)
		}
		if err != nil {
			return err
		}
	}
	var def any
	if p.DefaultRole != nil {
		def = ids[*p.DefaultRole]
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO auth_settings (id, default_role_id, updated_at) VALUES ('default', ?, ?)
		ON CONFLICT (id) DO UPDATE SET default_role_id = excluded.default_role_id, updated_at = excluded.updated_at`, def, ts); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearPolicy removes the policy; managed users block it.
func ClearPolicy(ctx context.Context, st *store.Store) (bool, error) {
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM auth_settings WHERE id = 'default'`).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	var user string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM auth_users WHERE status NOT IN ('removed') LIMIT 1`).Scan(&user); err == nil {
		return false, &AccessError{"policy-conflict", "Cannot clear the browser access policy while managed users exist."}
	}
	for _, q := range []string{`DELETE FROM auth_subject_role_bindings`, `DELETE FROM auth_email_role_bindings`,
		`DELETE FROM auth_domain_role_bindings`, `DELETE FROM auth_settings`, `DELETE FROM auth_roles`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// Identity is a verified browser identity.
type Identity struct {
	Issuer        string `json:"issuer"`
	Subject       string `json:"subject"`
	VerifiedEmail string `json:"verifiedEmail,omitempty"`
}

// Decision is what a policy grants an identity.
type Decision struct {
	MatchedRoles []string `json:"matchedRoles"`
	Permissions  []string `json:"permissions"`
}

// ResolvePolicy evaluates the policy for an identity; nil when no policy is
// configured. With claim, an invited (pending) user is linked to the
// identity on first sign-in with the invited email and activated.
func ResolvePolicy(ctx context.Context, st *store.Store, id Identity, claim bool) (*Decision, error) {
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var defaultID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT default_role_id FROM auth_settings WHERE id = 'default'`).Scan(&defaultID); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	email := strings.ToLower(id.VerifiedEmail)
	type storedIdentity struct {
		userID, status string
		email          sql.NullString
	}
	lookup := func() (*storedIdentity, error) {
		var s storedIdentity
		err := tx.QueryRowContext(ctx, `SELECT i.user_id, u.status, e.email FROM auth_identities i
			JOIN auth_users u ON i.user_id = u.id LEFT JOIN auth_user_emails e ON i.user_id = e.user_id
			WHERE i.issuer = ? AND i.subject = ?`, id.Issuer, id.Subject).Scan(&s.userID, &s.status, &s.email)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return &s, err
	}
	si, err := lookup()
	if err != nil {
		return nil, err
	}
	empty := &Decision{MatchedRoles: []string{}, Permissions: []string{}}
	if si == nil && email != "" && claim {
		var invited string
		if err := tx.QueryRowContext(ctx, `SELECT user_id FROM auth_user_emails WHERE email = ?`, email).Scan(&invited); err == nil {
			ts := now()
			if _, err := tx.ExecContext(ctx, `INSERT INTO auth_identities (id, user_id, issuer, subject, verified_email, created_at, last_seen_at)
				VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, store.UUID(), invited, id.Issuer, id.Subject, email, ts, ts); err != nil {
				return nil, err
			}
			if si, err = lookup(); err != nil {
				return nil, err
			}
			if si == nil || si.userID != invited {
				return empty, tx.Commit()
			}
			if si.status == "pending" {
				if _, err := tx.ExecContext(ctx, `UPDATE auth_users SET status = 'active', updated_at = ? WHERE id = ?`, ts, invited); err != nil {
					return nil, err
				}
				si.status = "active"
			}
		}
	}
	if si != nil && si.status == "pending" && claim && si.email.Valid && email == si.email.String {
		if _, err := tx.ExecContext(ctx, `UPDATE auth_users SET status = 'active', updated_at = ? WHERE id = ?`, now(), si.userID); err != nil {
			return nil, err
		}
		si.status = "active"
	}
	if si != nil && si.status != "active" {
		return empty, tx.Commit()
	}
	roleIDs := map[string]bool{}
	if defaultID.Valid {
		roleIDs[defaultID.String] = true
	}
	collect := func(query string, args ...any) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r string
			rows.Scan(&r)
			roleIDs[r] = true
		}
		return rows.Err()
	}
	if err := collect(`SELECT role_id FROM auth_subject_role_bindings WHERE issuer = ? AND subject = ?`, id.Issuer, id.Subject); err != nil {
		return nil, err
	}
	if email != "" {
		if err := collect(`SELECT role_id FROM auth_email_role_bindings WHERE email = ?`, email); err != nil {
			return nil, err
		}
		if at := strings.LastIndex(email, "@"); at >= 0 {
			if err := collect(`SELECT role_id FROM auth_domain_role_bindings WHERE domain = ?`, email[at+1:]); err != nil {
				return nil, err
			}
		}
	}
	if si != nil {
		if err := collect(`SELECT role_id FROM auth_user_roles WHERE user_id = ?`, si.userID); err != nil {
			return nil, err
		}
	}
	if len(roleIDs) == 0 {
		return empty, tx.Commit()
	}
	d := &Decision{MatchedRoles: []string{}}
	var perms []string
	for rid := range roleIDs {
		var name string
		if tx.QueryRowContext(ctx, `SELECT name FROM auth_roles WHERE id = ?`, rid).Scan(&name) == nil {
			d.MatchedRoles = append(d.MatchedRoles, name)
		}
		rows, err := tx.QueryContext(ctx, `SELECT permission FROM auth_role_permissions WHERE role_id = ?`, rid)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var p string
			rows.Scan(&p)
			perms = append(perms, p)
		}
		rows.Close()
	}
	sort.Strings(d.MatchedRoles)
	d.Permissions = Canonical(perms)
	return d, tx.Commit()
}
