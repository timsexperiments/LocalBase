package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// A real backup and the real command: the report names each role, what it gives, and who has it, so a
// file adding someone to a strong role isn't only a count going up by one.
func TestARestoreReportSaysWhatEachRoleGivesAndWhoHasIt(t *testing.T) {
	root := noAuthRoot(t)
	db, err := sql.Open("sqlite", filepath.Join(root, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO auth_roles (id, name, description, created_at, updated_at) VALUES ('r1', 'owner', 'x', 'x', 'x')`,
		`INSERT INTO auth_role_permissions (role_id, permission) VALUES ('r1', 'access:manage'), ('r1', 'keys:manage')`,
		`INSERT INTO auth_subject_role_bindings (id, role_id, issuer, subject) VALUES ('b1', 'r1', 'https://accounts.example.com', 'someone')`,
		`INSERT INTO auth_settings (id, default_role_id, updated_at) VALUES ('default', NULL, 'x')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	db.Close()
	file := filepath.Join(t.TempDir(), "b.lbbackup")
	if code, out := runApp(t, "--root", root, "backup", "--output", file); code != 0 {
		t.Fatalf("backup: %s", out)
	}
	code, out := runApp(t, "--root", filepath.Join(t.TempDir(), "target"), "restore", "--dry-run", "--no-port-probe", file)
	if code != 0 {
		t.Fatalf("restore: %s", out)
	}
	want := "What each role gives, and who has it:\n  owner: access:manage, keys:manage\n    subject someone at https://accounts.example.com\n"
	if !strings.Contains(out, want) {
		t.Errorf("no %q in:\n%s", want, out)
	}
}

func onDatabase(t *testing.T, root string, statements ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(root, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

// rolesRoot is a root with one role and one person on it, and a backup of it.
func rolesRoot(t *testing.T) (root, file string) {
	t.Helper()
	root = noAuthRoot(t)
	onDatabase(t, root,
		`INSERT INTO auth_roles (id, name, description, created_at, updated_at) VALUES ('r1', 'owner', 'x', 'x', 'x')`,
		`INSERT INTO auth_role_permissions (role_id, permission) VALUES ('r1', 'access:manage'), ('r1', 'keys:manage')`,
		`INSERT INTO auth_subject_role_bindings (id, role_id, issuer, subject) VALUES ('b1', 'r1', 'https://accounts.example.com', 'the-first')`,
		`INSERT INTO auth_settings (id, default_role_id, updated_at) VALUES ('default', NULL, 'x')`)
	file = filepath.Join(t.TempDir(), "b.lbbackup")
	if code, out := runApp(t, "--root", root, "backup", "--output", file); code != 0 {
		t.Fatalf("backup: %s", out)
	}
	return root, file
}

func dryRunOver(t *testing.T, root, file string) string {
	t.Helper()
	code, out := runApp(t, "--root", root, "restore", "--replace", "--dry-run", "--no-port-probe", file)
	if code != 0 {
		t.Fatalf("restore: %s", out)
	}
	return out
}

// What a file takes away is as much a change as what it adds. An owner locked out by it, and anyone
// added since an honest backup was made, need saying before the question.
func TestARestoreReportSaysWhoWouldLoseARole(t *testing.T) {
	root, file := rolesRoot(t)
	onDatabase(t, root, `INSERT INTO auth_subject_role_bindings (id, role_id, issuer, subject) VALUES ('b2', 'r1', 'https://accounts.example.com', 'added-later')`)
	out := dryRunOver(t, root, file)
	want := "  owner: access:manage, keys:manage\n    no longer has this role: subject added-later at https://accounts.example.com\n    subject the-first at https://accounts.example.com\n"
	if !strings.Contains(out, want) {
		t.Errorf("no %q in:\n%s", want, out)
	}
	if !strings.Contains(out, `This changes who has a role on this root. See "What each role gives" above.`) {
		t.Errorf("no caution:\n%s", out)
	}
}

func TestARestoreReportSaysWhichRolesWouldGo(t *testing.T) {
	root, file := rolesRoot(t)
	onDatabase(t, root,
		`INSERT INTO auth_roles (id, name, description, created_at, updated_at) VALUES ('r2', 'support', 'x', 'x', 'x')`,
		`INSERT INTO auth_role_permissions (role_id, permission) VALUES ('r2', 'models:read')`,
		`INSERT INTO auth_subject_role_bindings (id, role_id, issuer, subject) VALUES ('b2', 'r2', 'https://accounts.example.com', 'helper')`)
	out := dryRunOver(t, root, file)
	want := "What each role gives, and who has it:\n  No longer a role here: support (this root gives models:read)\n    no longer has this role: subject helper at https://accounts.example.com\n  owner: access:manage, keys:manage\n"
	if !strings.Contains(out, want) {
		t.Errorf("no %q in:\n%s", want, out)
	}
	if !strings.Contains(out, `This changes who has a role on this root. See "What each role gives" above.`) {
		t.Errorf("no caution:\n%s", out)
	}
}

// A file with no roles at all, over a root with one: the section still prints.
func TestARestoreReportSaysSoWhenTheFileHasNoRoles(t *testing.T) {
	root := noAuthRoot(t)
	file := filepath.Join(t.TempDir(), "b.lbbackup")
	if code, out := runApp(t, "--root", root, "backup", "--output", file); code != 0 {
		t.Fatalf("backup: %s", out)
	}
	onDatabase(t, root,
		`INSERT INTO auth_roles (id, name, description, created_at, updated_at) VALUES ('r1', 'owner', 'x', 'x', 'x')`,
		`INSERT INTO auth_role_permissions (role_id, permission) VALUES ('r1', 'keys:manage')`,
		`INSERT INTO auth_subject_role_bindings (id, role_id, issuer, subject) VALUES ('b1', 'r1', 'https://accounts.example.com', 'the-owner')`)
	out := dryRunOver(t, root, file)
	want := "What each role gives, and who has it:\n  No longer a role here: owner (this root gives keys:manage)\n    no longer has this role: subject the-owner at https://accounts.example.com\n"
	if !strings.Contains(out, want) {
		t.Errorf("no %q in:\n%s", want, out)
	}
	if !strings.Contains(out, `This changes who has a role on this root.`) {
		t.Errorf("no caution:\n%s", out)
	}
}

// A root last opened by an older version lacks the tables added since. It can be read, and it has
// nothing in those tables, so it mustn't be called unreadable or said to differ.
func TestARootWithoutTheNewerTablesIsNotCalledUnreadable(t *testing.T) {
	for _, table := range []string{"go_binding_issuers", "go_invitation_issuers"} {
		t.Run(table, func(t *testing.T) {
			root, file := rolesRoot(t)
			onDatabase(t, root, `DROP TABLE `+table)
			out := dryRunOver(t, root, file)
			if strings.Contains(out, "can't be read") {
				t.Errorf("called unreadable:\n%s", out)
			}
			if strings.Contains(out, "This changes who has a role") {
				t.Errorf("said to differ:\n%s", out)
			}
		})
	}
}

// From before those migrations: no access tables at all. Every role in the file is new to it, and
// that's the truth, so it says that and not that it couldn't read them.
func TestARootWithNoAccessTablesIsNotCalledUnreadable(t *testing.T) {
	root, file := rolesRoot(t)
	db, err := sql.Open("sqlite", filepath.Join(root, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND (name LIKE 'auth\_%' ESCAPE '\' OR name LIKE 'go\_%' ESCAPE '\')`)
	if err != nil {
		t.Fatal(err)
	}
	var drop []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name != "go_migrations" {
			drop = append(drop, `DROP TABLE `+name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	db.Close()
	if len(drop) < 8 {
		t.Fatalf("only %d tables to drop", len(drop))
	}
	onDatabase(t, root, drop...)
	out := dryRunOver(t, root, file)
	if strings.Contains(out, "can't be read") {
		t.Errorf("called unreadable:\n%s", out)
	}
	if !strings.Contains(out, "  owner: access:manage, keys:manage (new to this root)\n    subject the-first at https://accounts.example.com (new to this root)\n") {
		t.Errorf("the file's role isn't new to it:\n%s", out)
	}
}
