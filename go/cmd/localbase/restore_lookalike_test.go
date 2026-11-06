package main

import (
	"database/sql"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// signInRoot is a root with these places in its sign-in settings.
func signInRoot(t *testing.T, issuer, origin, mailHost string) string {
	t.Helper()
	root := noAuthRoot(t)
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("ui-access.json", `{"provider":{"kind":"direct","registrations":[
		{"kind":"oidc","id":"google","name":"Google","issuer":"`+issuer+`","clientId":"cid","clientAuthentication":{"kind":"none"}}
	]},"origin":"`+origin+`","permissions":[]}`)
	write("email-delivery.json", `{"host":"`+mailHost+`","port":587,"security":"starttls","authentication":{"kind":"none"},"from":"a@example.com"}`)
	return root
}

// A letter from another alphabet can make one address look like another, and the report is where a
// person judges these by eye. So a value that names a place shows anything outside ASCII as an escape.
// --json keeps it as it is.
func TestALookalikeAddressIsShownForWhatItIs(t *testing.T) {
	theirs := signInRoot(t, "https://\u0430ccounts.example.com", "https://l\u043ecalbase.example.com", "smtp.\u0435xample.com")
	file := filepath.Join(t.TempDir(), "b.lbbackup")
	if code, out := runApp(t, "--root", theirs, "backup", "--output", file); code != 0 {
		t.Fatalf("backup: %s", out)
	}
	mine := signInRoot(t, "https://accounts.example.com", "https://localbase.example.com", "smtp.example.com")
	code, out := runApp(t, "--root", mine, "restore", "--dry-run", "--no-port-probe", "--replace", file)
	if code != 0 {
		t.Fatalf("restore: %s", out)
	}
	for _, want := range []string{
		`https://\u0430ccounts.example.com (this file names it oidc:google; this root has https://accounts.example.com)`,
		`Address people sign in at: https://l\u043ecalbase.example.com (this root has https://localbase.example.com)`,
		`Mail server, which can read sign-in links: smtp.\u0435xample.com:587 (this root has smtp.example.com:587)`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	code, asJSON := runApp(t, "--json", "--root", mine, "restore", "--dry-run", "--no-port-probe", "--replace", file)
	if code != 0 {
		t.Fatalf("restore --json: %s", asJSON)
	}
	if !strings.Contains(asJSON, "https://\u0430ccounts.example.com") {
		t.Errorf("--json lost the real value: %.500s", asJSON)
	}
}

// The other way round: it's this root that has the lookalike, and the note about it shows that.
func TestALookalikeOnThisRootIsShownForWhatItIs(t *testing.T) {
	theirs := signInRoot(t, "https://accounts.example.com", "https://localbase.example.com", "smtp.example.com")
	file := filepath.Join(t.TempDir(), "b.lbbackup")
	if code, out := runApp(t, "--root", theirs, "backup", "--output", file); code != 0 {
		t.Fatalf("backup: %s", out)
	}
	mine := signInRoot(t, "https://\u0430ccounts.example.com", "https://localbase.example.com", "smtp.example.com")
	code, out := runApp(t, "--root", mine, "restore", "--dry-run", "--no-port-probe", "--replace", file)
	if code != 0 {
		t.Fatalf("restore: %s", out)
	}
	if want := `this root has https://\u0430ccounts.example.com`; !strings.Contains(out, want) {
		t.Errorf("no %q in:\n%s", want, out)
	}
}

// The lines the report prints from elsewhere, through the real command: a binding's issuer is its own
// value in the database, separate from the sign-in settings, and a program's path is where a person
// decides what runs as them.
func TestLookalikesInBindingsAndProgramsAreShownForWhatTheyAre(t *testing.T) {
	const lookalike = "https://\u0430ccounts.example.com"
	const shown = `https://\u0430ccounts.example.com`
	theirs := signInRoot(t, "https://accounts.example.com", "https://localbase.example.com", "smtp.example.com")
	db, err := sql.Open("sqlite", filepath.Join(theirs, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO auth_roles (id, name, description, created_at, updated_at) VALUES ('r1', 'owner', 'x', 'x', 'x')`,
		`INSERT INTO auth_role_permissions (role_id, permission) VALUES ('r1', 'access:manage')`,
		`INSERT INTO auth_subject_role_bindings (id, role_id, issuer, subject) VALUES ('b1', 'r1', '` + lookalike + `', 'someone')`,
		`INSERT INTO auth_settings (id, default_role_id, updated_at) VALUES ('default', NULL, 'x')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	db.Close()
	backend := filepath.Join(theirs, "backends.d", "custom.json")
	if err := os.MkdirAll(filepath.Dir(backend), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backend, []byte(strings.Replace(backendWithEscapes, `"run"`, `"/usr/bin/\u0430run"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "b.lbbackup")
	if code, out := runApp(t, "--root", theirs, "backup", "--output", file); code != 0 {
		t.Fatalf("backup: %s", out)
	}
	mine := signInRoot(t, "https://accounts.example.com", "https://localbase.example.com", "smtp.example.com")
	code, out := runApp(t, "--root", mine, "restore", "--dry-run", "--no-port-probe", "--replace", file)
	if code != 0 {
		t.Fatalf("restore: %s", out)
	}
	for name, want := range map[string]string{
		"a binding's issuer":                "  " + shown + ": 1 role bindings",
		"a binding this root no longer has": "would come back: " + shown + ": 1.",
		"a program's argument":              `/usr/bin/\u0430run`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("%s: no %q in:\n%s", name, want, out)
		}
	}
	if strings.Contains(out, lookalike) {
		t.Errorf("the lookalike reached the terminal:\n%s", out)
	}
}

// A person's own path has to read as they'd type it, in the lines that give them a command to copy.
func TestAPersonsOwnPathKeepsItsLetters(t *testing.T) {
	// A root is named by its real path, and a temporary folder can be behind a link.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "jos\u00e9", "root")
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	theirs := noAuthRoot(t)
	file := filepath.Join(t.TempDir(), "b.lbbackup")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	busy := ln.Addr().(*net.TCPAddr).Port
	if code, out := runApp(t, "--root", theirs, "configure", "--defaults", "--no-create-key", "--port", strconv.Itoa(busy)); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	if code, out := runApp(t, "--root", theirs, "backup", "--output", file); code != 0 {
		t.Fatalf("backup: %s", out)
	}
	code, out := runApp(t, "--root", root, "restore", "--dry-run", file)
	if code != 0 {
		t.Fatalf("restore: %s", out)
	}
	if want := "local-base --root '" + root + "'"; !strings.Contains(out, want) {
		t.Errorf("no %q in:\n%s", want, out)
	}
}
