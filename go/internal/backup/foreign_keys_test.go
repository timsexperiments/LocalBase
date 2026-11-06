package backup

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// A row pointing at something that isn't there can't come from LocalBase, which keeps foreign keys
// on, and what reads it may not look at what it points to. So a restore refuses it rather than
// repairing it, and names the table and how many rows, not what's in them.
func TestRestoreRefusesARowPointingAtNothing(t *testing.T) {
	const marker = "planted-row-contents"
	for _, c := range []struct {
		name, table string
		rows        []string
	}{
		{"a role's permissions", "auth_role_permissions", []string{
			`INSERT INTO auth_role_permissions (role_id, permission) VALUES ('` + marker + `', 'access:manage'), ('` + marker + `', 'keys:manage')`}},
		{"the default role", "auth_settings", []string{
			`INSERT INTO auth_settings (id, default_role_id, updated_at) VALUES ('default', '` + marker + `', 'x') ON CONFLICT (id) DO UPDATE SET default_role_id = excluded.default_role_id`}},
		{"a subject's binding", "auth_subject_role_bindings", []string{
			`INSERT INTO auth_subject_role_bindings (id, role_id, issuer, subject) VALUES ('b', '` + marker + `', 'https://id.example.com', 's')`}},
		{"an email's binding", "auth_email_role_bindings", []string{
			`INSERT INTO auth_email_role_bindings (id, role_id, email) VALUES ('b', '` + marker + `', 'a@example.com')`}},
		{"a domain's binding", "auth_domain_role_bindings", []string{
			`INSERT INTO auth_domain_role_bindings (id, role_id, domain) VALUES ('b', '` + marker + `', 'example.com')`}},
		{"a person's role", "auth_user_roles", []string{
			`INSERT INTO auth_user_roles (user_id, role_id) VALUES ('user-1', '` + marker + `')`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, _ := newRoot(t, dir(t))
			e := read(t, create(t, r, false).Output)
			editDB(t, e, append([]string{`PRAGMA foreign_keys=OFF`}, c.rows...)...)
			crafted := filepath.Join(dir(t), "crafted"+Extension)
			pack(t, crafted, e)
			target := filepath.Join(dir(t), "target")
			s, err := Stage(context.Background(), crafted, target, false)
			if err == nil {
				s.Discard()
				t.Fatal("it was accepted")
			}
			if !strings.Contains(err.Error(), c.table) {
				t.Errorf("it doesn't name the table: %v", err)
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("it repeats what's in the row: %v", err)
			}
		})
	}
}

func TestRestoreSaysHowManyRowsPointAtNothing(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	e := read(t, create(t, r, false).Output)
	editDB(t, e, `PRAGMA foreign_keys=OFF`,
		`INSERT INTO auth_role_permissions (role_id, permission) VALUES ('ghost', 'access:manage'), ('ghost', 'keys:manage'), ('ghost', 'models:manage')`,
		`INSERT INTO auth_subject_role_bindings (id, role_id, issuer, subject) VALUES ('b', 'ghost', 'https://id.example.com', 's')`)
	crafted := filepath.Join(dir(t), "crafted"+Extension)
	pack(t, crafted, e)
	_, err := Stage(context.Background(), crafted, filepath.Join(dir(t), "target"), false)
	if err == nil {
		t.Fatal("it was accepted")
	}
	for _, want := range []string{"auth_role_permissions: 3", "auth_subject_role_bindings: 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("no %q in: %v", want, err)
		}
	}
}

// A table rebuilt without its foreign key would hold such rows with nothing to catch them, so the
// foreign key has to be there.
func TestRestoreRefusesATableRebuiltWithoutItsForeignKey(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	e := read(t, create(t, r, false).Output)
	editDB(t, e, `PRAGMA foreign_keys=OFF`,
		`CREATE TABLE new_permissions (role_id text NOT NULL, permission text NOT NULL, PRIMARY KEY(role_id, permission))`,
		`INSERT INTO new_permissions SELECT * FROM auth_role_permissions`,
		`DROP TABLE auth_role_permissions`,
		`ALTER TABLE new_permissions RENAME TO auth_role_permissions`,
		`INSERT INTO auth_role_permissions (role_id, permission) VALUES ('ghost', 'access:manage')`)
	crafted := filepath.Join(dir(t), "crafted"+Extension)
	pack(t, crafted, e)
	s, err := Stage(context.Background(), crafted, filepath.Join(dir(t), "target"), false)
	if err == nil {
		s.Discard()
		t.Fatal("it was accepted")
	}
	if !strings.Contains(err.Error(), "auth_role_permissions.role_id") {
		t.Errorf("it doesn't name the foreign key: %v", err)
	}
}

// A check over a database that only LocalBase wrote finds nothing, so it can't turn away what LocalBase made.
func TestABackupLocalBaseMadeHasNoRowPointingAtNothing(t *testing.T) {
	for _, exclude := range []bool{false, true} {
		r, _ := newRoot(t, dir(t))
		target := filepath.Join(dir(t), "target")
		s, err := Stage(context.Background(), create(t, r, exclude).Output, target, false)
		if err != nil {
			t.Fatalf("without secrets: %v: %v", exclude, err)
		}
		s.Discard()
	}
}

// A role that's really there is left alone, with rows of its own, and the policy still has to pass.
func TestARealRoleIsNotARowPointingAtNothing(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	e := read(t, create(t, r, false).Output)
	editDB(t, e,
		`INSERT INTO auth_roles (id, name, description, created_at, updated_at) VALUES ('owner', 'owner', 'Owner', 'x', 'x')`,
		`INSERT INTO auth_role_permissions (role_id, permission) VALUES ('owner', 'access:manage')`,
		`INSERT INTO auth_subject_role_bindings (id, role_id, issuer, subject) VALUES ('b', 'owner', 'https://accounts.example.com', 's')`)
	crafted := filepath.Join(dir(t), "crafted"+Extension)
	pack(t, crafted, e)
	s, err := Stage(context.Background(), crafted, filepath.Join(dir(t), "target"), false)
	if err != nil {
		t.Fatal(err)
	}
	s.Discard()
}
