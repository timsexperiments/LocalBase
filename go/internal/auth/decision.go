package auth

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

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
//
// It runs on every request from a signed-in person, and nearly always only reads, so it looks first
// without the write lock. Only a sign-in that has something to claim goes round again with it.
func ResolvePolicy(ctx context.Context, st *store.Store, id Identity, claim bool) (*Decision, error) {
	d, err := evaluatePolicy(ctx, st, id, claim, &sql.TxOptions{ReadOnly: true})
	if errors.Is(err, errPolicyWrites) {
		return evaluatePolicy(ctx, st, id, claim, nil)
	}
	return d, err
}

// errPolicyWrites: this identity has an invitation to claim, which a read can't do.
var errPolicyWrites = errors.New("the policy has something to write")

// evaluatePolicy is ResolvePolicy in one transaction. With read-only options, a claim stops it.
func evaluatePolicy(ctx context.Context, st *store.Store, id Identity, claim bool, opts *sql.TxOptions) (*Decision, error) {
	readOnly := opts != nil && opts.ReadOnly
	tx, err := st.DB.BeginTx(ctx, opts)
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
	if si == nil && email != "" {
		var invited, status string
		var invitedBy sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT e.user_id, u.status, i.issuer FROM auth_user_emails e JOIN auth_users u ON e.user_id = u.id
			LEFT JOIN go_invitation_issuers i ON i.user_id = e.user_id WHERE e.email = ?`, email).Scan(&invited, &status, &invitedBy)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		// A disabled or removed address stays out, whichever provider vouches for it.
		if err == nil && status != "active" && status != "pending" {
			return empty, tx.Commit()
		}
		// Only the provider a person was invited through can claim their record.
		if err == nil && claim && invitedBy.Valid && invitedBy.String == id.Issuer {
			if readOnly {
				return nil, errPolicyWrites
			}
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
		if readOnly {
			return nil, errPolicyWrites
		}
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
			if err := rows.Scan(&r); err != nil {
				return err
			}
			roleIDs[r] = true
		}
		return rows.Err()
	}
	if err := collect(`SELECT role_id FROM auth_subject_role_bindings WHERE issuer = ? AND subject = ?`, id.Issuer, id.Subject); err != nil {
		return nil, err
	}
	if email != "" {
		if err := collect(`SELECT b.role_id FROM auth_email_role_bindings b JOIN go_binding_issuers i
			ON i.kind = 'email' AND i.role_id = b.role_id AND i.value = b.email WHERE b.email = ? AND i.issuer = ?`, email, id.Issuer); err != nil {
			return nil, err
		}
		if at := strings.LastIndex(email, "@"); at >= 0 {
			if err := collect(`SELECT b.role_id FROM auth_domain_role_bindings b JOIN go_binding_issuers i
				ON i.kind = 'email-domain' AND i.role_id = b.role_id AND i.value = b.domain WHERE b.domain = ? AND i.issuer = ?`, email[at+1:], id.Issuer); err != nil {
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
		if err := tx.QueryRowContext(ctx, `SELECT name FROM auth_roles WHERE id = ?`, rid).Scan(&name); errors.Is(err, sql.ErrNoRows) {
			continue // A role that doesn't exist grants nothing, whatever rows still name it.
		} else if err != nil {
			return nil, err
		}
		d.MatchedRoles = append(d.MatchedRoles, name)
		rows, err := tx.QueryContext(ctx, `SELECT permission FROM auth_role_permissions WHERE role_id = ?`, rid)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return nil, err
			}
			perms = append(perms, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	sort.Strings(d.MatchedRoles)
	d.Permissions = Canonical(perms)
	return d, tx.Commit()
}
