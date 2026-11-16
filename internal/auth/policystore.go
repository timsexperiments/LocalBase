package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"

	"github.com/timsexperiments/LocalBase/go/internal/collate"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

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
	for rows.Next() {
		var id string
		var r Role
		if err := rows.Scan(&id, &r.Name, &r.Description); err != nil {
			rows.Close()
			return nil, err
		}
		names[id] = r.Name
		roles = append(roles, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	perms := map[string][]string{}
	if rows, err = db.QueryContext(ctx, `SELECT role_id, permission FROM auth_role_permissions`); err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, perm string
		if err := rows.Scan(&id, &perm); err != nil {
			rows.Close()
			return nil, err
		}
		perms[names[id]] = append(perms[names[id]], perm)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
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
			if err := rows.Scan(&roleID, &a, &b); err != nil {
				return err
			}
			p.Bindings = append(p.Bindings, build(names[roleID], a, b))
		}
		return rows.Err()
	}
	if err := scan(`SELECT role_id, issuer, subject FROM auth_subject_role_bindings`, func(r, a, b string) Binding {
		return Binding{Kind: "subject", Role: r, Issuer: a, Subject: b}
	}); err != nil {
		return nil, err
	}
	if err := scan(`SELECT b.role_id, b.email, COALESCE(i.issuer, '') FROM auth_email_role_bindings b
		LEFT JOIN go_binding_issuers i ON i.kind = 'email' AND i.role_id = b.role_id AND i.value = b.email`, func(r, a, issuer string) Binding {
		return Binding{Kind: "email", Role: r, Email: a, Issuer: issuer}
	}); err != nil {
		return nil, err
	}
	if err := scan(`SELECT b.role_id, b.domain, COALESCE(i.issuer, '') FROM auth_domain_role_bindings b
		LEFT JOIN go_binding_issuers i ON i.kind = 'email-domain' AND i.role_id = b.role_id AND i.value = b.domain`, func(r, a, issuer string) Binding {
		return Binding{Kind: "email-domain", Role: r, Domain: a, Issuer: issuer}
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
		if err := rows.Scan(&id, &name); err != nil {
			_ = rows.Close()
			return err
		}
		existing[name] = id
	}
	// A role that wasn't read would look new, and get a new id, leaving who holds it behind.
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
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
		switch {
		case err == nil:
			return &AccessError{"policy-conflict", "Cannot remove browser access role " + held + " while it is assigned to a managed user."}
		case !errors.Is(err, sql.ErrNoRows):
			return err // read as "nobody holds it", the role would go, and whoever holds it with it
		}
	}
	for _, q := range []string{`DELETE FROM auth_subject_role_bindings`, `DELETE FROM auth_email_role_bindings`,
		`DELETE FROM auth_domain_role_bindings`, `DELETE FROM go_binding_issuers`, `DELETE FROM auth_role_permissions`} {
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
			// One row per address and role; each provider vouching for it adds a name.
			if _, err = tx.ExecContext(ctx, `INSERT INTO auth_email_role_bindings (id, role_id, email) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
				bindingID(b.Kind, roleID, b.Email), roleID, b.Email); err == nil {
				err = nameBindingIssuer(ctx, tx, b.Kind, roleID, b.Email, b.Issuer)
			}
		case "email-domain":
			if _, err = tx.ExecContext(ctx, `INSERT INTO auth_domain_role_bindings (id, role_id, domain) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
				bindingID(b.Kind, roleID, b.Domain), roleID, b.Domain); err == nil {
				err = nameBindingIssuer(ctx, tx, b.Kind, roleID, b.Domain, b.Issuer)
			}
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

func nameBindingIssuer(ctx context.Context, tx *sql.Tx, kind, roleID, value, issuer string) error {
	if issuer == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO go_binding_issuers (kind, role_id, value, issuer) VALUES (?, ?, ?, ?)`, kind, roleID, value, issuer)
	return err
}

// ClearPolicy removes the policy; managed users block it.
func ClearPolicy(ctx context.Context, st *store.Store) (bool, error) {
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var id string
	switch err := tx.QueryRowContext(ctx, `SELECT id FROM auth_settings WHERE id = 'default'`).Scan(&id); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}
	var user string
	switch err := tx.QueryRowContext(ctx, `SELECT id FROM auth_users WHERE status NOT IN ('removed') LIMIT 1`).Scan(&user); {
	case err == nil:
		return false, &AccessError{"policy-conflict", "Cannot clear the browser access policy while managed users exist."}
	case !errors.Is(err, sql.ErrNoRows):
		return false, err // read as "nobody", every role and binding would go
	}
	for _, q := range []string{`DELETE FROM auth_subject_role_bindings`, `DELETE FROM auth_email_role_bindings`,
		`DELETE FROM auth_domain_role_bindings`, `DELETE FROM go_binding_issuers`, `DELETE FROM auth_settings`, `DELETE FROM auth_roles`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}
