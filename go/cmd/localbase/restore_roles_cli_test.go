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
