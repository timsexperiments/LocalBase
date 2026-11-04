package auth

import (
	"context"
	"reflect"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// plant writes rows with foreign keys off, as an edited copy of the database could hold them. In
// normal use the database won't take them.
func plant(t *testing.T, st *store.Store, statements ...string) {
	t.Helper()
	ctx := context.Background()
	conn, err := st.DB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`) //nolint:errcheck // the connection is about to go back
	for _, stmt := range statements {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

// A role that doesn't exist grants nothing, whichever row names it. The sign-in check reads these
// rows on every sign-in, so this holds there as well as at a restore.
func TestARoleThatDoesNotExistGrantsNothing(t *testing.T) {
	const issuer, subject, email = "https://id.example.com", "someone", "someone@example.com"
	someone := Identity{Issuer: issuer, Subject: subject, VerifiedEmail: email}
	permissions := `INSERT INTO auth_role_permissions (role_id, permission) VALUES ('ghost', 'access:manage'), ('ghost', 'keys:manage')`
	for _, c := range []struct {
		name    string
		rows    []string
		as      Identity
		claimed bool
	}{
		{name: "the default role", rows: []string{`UPDATE auth_settings SET default_role_id = 'ghost' WHERE id = 'default'`}, as: Identity{Issuer: issuer, Subject: "anyone at all"}},
		{name: "a subject's binding", rows: []string{`INSERT INTO auth_subject_role_bindings (id, role_id, issuer, subject) VALUES ('b', 'ghost', '` + issuer + `', '` + subject + `')`}, as: someone},
		{name: "an email's binding", rows: []string{
			`INSERT INTO auth_email_role_bindings (id, role_id, email) VALUES ('b', 'ghost', '` + email + `')`,
			`INSERT INTO go_binding_issuers (kind, role_id, value, issuer) VALUES ('email', 'ghost', '` + email + `', '` + issuer + `')`}, as: someone},
		{name: "a domain's binding", rows: []string{
			`INSERT INTO auth_domain_role_bindings (id, role_id, domain) VALUES ('b', 'ghost', 'example.com')`,
			`INSERT INTO go_binding_issuers (kind, role_id, value, issuer) VALUES ('email-domain', 'ghost', 'example.com', '` + issuer + `')`}, as: someone},
		{name: "a person's role", rows: []string{
			`INSERT INTO auth_users (id, status, created_at, updated_at) VALUES ('u', 'active', 'x', 'x')`,
			`INSERT INTO auth_identities (id, user_id, issuer, subject, verified_email, created_at, last_seen_at) VALUES ('i', 'u', '` + issuer + `', '` + subject + `', '` + email + `', 'x', 'x')`,
			`INSERT INTO auth_user_roles (user_id, role_id) VALUES ('u', 'ghost')`}, as: someone},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := testStore(t)
			ctx := context.Background()
			if err := ApplyPolicy(ctx, st, &Policy{Roles: []Role{{Name: "owner", Description: "Owner", Permissions: []string{"access:manage"}}},
				Bindings: []Binding{{Kind: "subject", Role: "owner", Issuer: issuer, Subject: "the-owner"}}}); err != nil {
				t.Fatal(err)
			}
			plant(t, st, append([]string{permissions}, c.rows...)...)
			d, err := ResolvePolicy(ctx, st, c.as, false)
			if err != nil {
				t.Fatal(err)
			}
			if d == nil || len(d.Permissions) != 0 || len(d.MatchedRoles) != 0 {
				t.Errorf("it grants: %+v", d)
			}
			// The role that does exist still works, so this isn't refusing everything.
			d, err = ResolvePolicy(ctx, st, Identity{Issuer: issuer, Subject: "the-owner"}, false)
			if err != nil {
				t.Fatal(err)
			}
			if d == nil || !reflect.DeepEqual(d.Permissions, []string{"access:manage"}) {
				t.Errorf("the real role: %+v", d)
			}
		})
	}
}

// Not being able to read a role isn't the same as it not existing, and it doesn't grant anything.
func TestAFailureReadingARoleIsAnError(t *testing.T) {
	const issuer = "https://id.example.com"
	st := testStore(t)
	ctx := context.Background()
	if err := ApplyPolicy(ctx, st, &Policy{Roles: []Role{{Name: "owner", Description: "Owner", Permissions: []string{"access:manage"}}},
		Bindings: []Binding{{Kind: "subject", Role: "owner", Issuer: issuer, Subject: "the-owner"}}}); err != nil {
		t.Fatal(err)
	}
	plant(t, st, `PRAGMA legacy_alter_table=ON`, `ALTER TABLE auth_roles RENAME TO auth_roles_gone`)
	d, err := ResolvePolicy(ctx, st, Identity{Issuer: issuer, Subject: "the-owner"}, false)
	if err == nil {
		t.Errorf("no error, and it grants: %+v", d)
	}
}

// Beside a real role, a missing one adds nothing and takes nothing away.
func TestAMissingRoleLeavesARealOneAlone(t *testing.T) {
	const issuer = "https://id.example.com"
	st := testStore(t)
	ctx := context.Background()
	if err := ApplyPolicy(ctx, st, &Policy{Roles: []Role{{Name: "owner", Description: "Owner", Permissions: []string{"access:manage"}},
		{Name: "chat", Description: "Chat", Permissions: []string{"inference:chat"}}},
		Bindings: []Binding{{Kind: "subject", Role: "owner", Issuer: issuer, Subject: "the-owner"}, {Kind: "subject", Role: "chat", Issuer: issuer, Subject: "someone"}}}); err != nil {
		t.Fatal(err)
	}
	plant(t, st, `INSERT INTO auth_role_permissions (role_id, permission) VALUES ('ghost', 'keys:manage')`,
		`INSERT INTO auth_subject_role_bindings (id, role_id, issuer, subject) VALUES ('b', 'ghost', '`+issuer+`', 'someone')`)
	d, err := ResolvePolicy(ctx, st, Identity{Issuer: issuer, Subject: "someone"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if d == nil || !reflect.DeepEqual(d.Permissions, []string{"inference:chat"}) || !reflect.DeepEqual(d.MatchedRoles, []string{"chat"}) {
		t.Errorf("%+v", d)
	}
}
