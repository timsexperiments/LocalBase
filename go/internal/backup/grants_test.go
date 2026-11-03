package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

const issuerA = "https://accounts.example.com"

var ownerRole = []string{
	`INSERT INTO auth_roles (id, name, description, created_at, updated_at) VALUES ('owner', 'owner', 'x', 'x', 'x')`,
	`INSERT INTO auth_role_permissions (role_id, permission) VALUES ('owner', 'keys:manage'), ('owner', 'access:manage')`,
}

func grantsIn(t *testing.T, statements ...string) []Grant {
	t.Helper()
	st, err := store.Open(filepath.Join(dir(t), "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, stmt := range append(append([]string{}, ownerRole...), statements...) {
		if _, err := st.DB.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	grants, err := readGrants(context.Background(), st.DB)
	if err != nil {
		t.Fatal(err)
	}
	return grants
}

// Each way of having a role reads as what it is.
func TestEachWayOfHavingARoleIsNamed(t *testing.T) {
	person := func(id, status string) []string {
		return []string{`INSERT INTO auth_users (id, status, created_at, updated_at) VALUES ('` + id + `', '` + status + `', 'x', 'x')`,
			`INSERT INTO auth_user_roles (user_id, role_id) VALUES ('` + id + `', 'owner')`,
			`INSERT INTO auth_user_emails (user_id, email) VALUES ('` + id + `', '` + id + `@example.com')`}
	}
	for _, c := range []struct {
		name string
		rows []string
		want string
	}{
		{"a subject", []string{`INSERT INTO auth_subject_role_bindings (id, role_id, issuer, subject) VALUES ('b', 'owner', '` + issuerA + `', 'someone')`},
			"someone at " + issuerA},
		{"an address", []string{`INSERT INTO auth_email_role_bindings (id, role_id, email) VALUES ('b', 'owner', 'a@example.com')`,
			`INSERT INTO go_binding_issuers (kind, role_id, value, issuer) VALUES ('email', 'owner', 'a@example.com', '` + issuerA + `')`},
			"a@example.com, signing in through " + issuerA},
		{"an address with no provider", []string{`INSERT INTO auth_email_role_bindings (id, role_id, email) VALUES ('b', 'owner', 'a@example.com')`},
			"a@example.com, once a sign-in provider is chosen"},
		{"a domain", []string{`INSERT INTO auth_domain_role_bindings (id, role_id, domain) VALUES ('b', 'owner', 'example.com')`,
			`INSERT INTO go_binding_issuers (kind, role_id, value, issuer) VALUES ('email-domain', 'owner', 'example.com', '` + issuerA + `')`},
			"anyone at example.com, signing in through " + issuerA},
		{"someone who has signed in", append(person("u", "active"),
			`INSERT INTO auth_identities (id, user_id, issuer, subject, verified_email, created_at, last_seen_at) VALUES ('i', 'u', '`+issuerA+`', 'them', NULL, 'x', 'x')`),
			"them at " + issuerA},
		{"someone invited", append(person("u", "pending"),
			`INSERT INTO go_invitation_issuers (user_id, issuer) VALUES ('u', '`+issuerA+`')`),
			"u@example.com, invited, signing in through " + issuerA},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := grantsIn(t, c.rows...)
			if len(g) != 1 || g[0].Role != "owner" || g[0].Gives.Value != "access:manage, keys:manage" {
				t.Fatalf("%+v", g)
			}
			if len(g[0].Who) != 1 || g[0].Who[0].Value != c.want {
				t.Errorf("%+v", g[0].Who)
			}
		})
	}
}

// A person an admin has disabled or removed isn't reached by the role, so isn't listed.
func TestSomeoneWhoCannotUseARoleIsNotListed(t *testing.T) {
	for _, status := range []string{"disabled", "removed"} {
		g := grantsIn(t,
			`INSERT INTO auth_users (id, status, created_at, updated_at) VALUES ('u', '`+status+`', 'x', 'x')`,
			`INSERT INTO auth_user_roles (user_id, role_id) VALUES ('u', 'owner')`,
			`INSERT INTO auth_identities (id, user_id, issuer, subject, verified_email, created_at, last_seen_at) VALUES ('i', 'u', '`+issuerA+`', 'them', NULL, 'x', 'x')`)
		if len(g[0].Who) != 0 {
			t.Errorf("%s: %+v", status, g[0].Who)
		}
	}
}

// The read works on any database, including one that isn't what's being restored.
func TestAFailedReadOfRolesIsAnError(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(dir(t), "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := readGrants(context.Background(), db); err == nil {
		t.Error("no error from a database with no tables")
	}
}

func withRole(t *testing.T, extra ...string) (rootDir, file string) {
	t.Helper()
	r, _ := newRoot(t, dir(t))
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, stmt := range append(append(append([]string{}, ownerRole...),
		`INSERT INTO auth_subject_role_bindings (id, role_id, issuer, subject) VALUES ('b1', 'owner', '`+issuerA+`', 'the-owner')`,
		`INSERT INTO auth_settings (id, default_role_id, updated_at) VALUES ('default', NULL, 'x')`), extra...) {
		if _, err := st.DB.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	return r.Dir, create(t, r, false).Output
}

func editedBackup(t *testing.T, file string, statements ...string) string {
	t.Helper()
	e := read(t, file)
	editDB(t, e, statements...)
	out := filepath.Join(dir(t), "crafted"+Extension)
	pack(t, out, e)
	return out
}

func caution(rep *Report, want string) bool {
	for _, c := range rep.Cautions {
		if c == want {
			return true
		}
	}
	return false
}

// A file adding someone to a strong role: named, marked, first in the list, and cautioned.
func TestSomeoneNewToARoleIsMarked(t *testing.T) {
	root, file := withRole(t)
	bad := editedBackup(t, file,
		`INSERT INTO auth_subject_role_bindings (id, role_id, issuer, subject) VALUES ('b2', 'owner', 'https://z.example.com', 'zz-added')`)
	rep := stageReport(t, bad, root, true)
	g := rep.Grants[0]
	if len(g.Who) != 2 || g.Who[0].Value != "zz-added at https://z.example.com" || g.Who[0].Against != AgainstNew {
		t.Fatalf("%+v", g.Who)
	}
	if g.Who[1].Value != "the-owner at "+issuerA || g.Who[1].Against != "" {
		t.Errorf("%+v", g.Who[1])
	}
	if !caution(rep, grantsChangedCaution) {
		t.Errorf("no caution: %v", rep.Cautions)
	}
}

// A role giving more than this root's, under the same name.
func TestARoleGivingSomethingElseIsSaid(t *testing.T) {
	root, file := withRole(t)
	bad := editedBackup(t, file, `INSERT INTO auth_role_permissions (role_id, permission) VALUES ('owner', 'models:manage')`)
	rep := stageReport(t, bad, root, true)
	got := rep.Grants[0].Gives
	if got.Against != AgainstDiffers || got.ThisRoot != "access:manage, keys:manage" || got.Value != "access:manage, keys:manage, models:manage" {
		t.Errorf("%+v", got)
	}
	if !caution(rep, grantsChangedCaution) {
		t.Errorf("no caution: %v", rep.Cautions)
	}
}

func TestARoleNewToThisRootIsSaid(t *testing.T) {
	root, file := withRole(t)
	bad := editedBackup(t, file,
		`INSERT INTO auth_roles (id, name, description, created_at, updated_at) VALUES ('new', 'new-role', 'x', 'x', 'x')`,
		`INSERT INTO auth_role_permissions (role_id, permission) VALUES ('new', 'keys:manage')`,
		`INSERT INTO auth_subject_role_bindings (id, role_id, issuer, subject) VALUES ('b2', 'new', '`+issuerA+`', 'added')`)
	rep := stageReport(t, bad, root, true)
	var g *Grant
	for i := range rep.Grants {
		if rep.Grants[i].Role == "new-role" {
			g = &rep.Grants[i]
		}
	}
	if g == nil || g.Gives.Against != AgainstNew || g.Who[0].Against != AgainstNew {
		t.Fatalf("%+v", rep.Grants)
	}
}

func TestTheSameRolesSayNothingMore(t *testing.T) {
	root, file := withRole(t)
	rep := stageReport(t, file, root, true)
	if rep.Grants[0].Gives.Against != "" || rep.Grants[0].Who[0].Against != "" || caution(rep, grantsChangedCaution) {
		t.Errorf("%+v, %v", rep.Grants, rep.Cautions)
	}
}

func TestANewRootComparesRolesWithNothing(t *testing.T) {
	_, file := withRole(t)
	rep := stageReport(t, file, filepath.Join(dir(t), "target"), false)
	if rep.Grants[0].Gives.Against != "" || rep.Grants[0].Who[0].Against != "" || caution(rep, grantsChangedCaution) {
		t.Errorf("%+v, %v", rep.Grants, rep.Cautions)
	}
}

func TestARoleThatGivesNothingOnThisRootSaysSo(t *testing.T) {
	grants := []Grant{{Role: "owner", Gives: Line{Value: "keys:manage"}, Who: []Line{}}}
	if !compareGrants(grants, []Grant{{Role: "owner", Who: []Line{}}}, nil) {
		t.Error("it didn't count as a change")
	}
	if got := grants[0].Gives.Text("gives"); got != "keys:manage (this root gives nothing)" {
		t.Errorf("%q", got)
	}
}

// What this root has can't be read: that's said, and it isn't taken for having none.
func TestRolesThisRootHasBeingUnreadableIsSaid(t *testing.T) {
	grants := []Grant{{Role: "owner", Gives: Line{Value: "keys:manage"}, Who: []Line{{Value: "someone at x"}}}}
	if !compareGrants(grants, nil, context.Canceled) {
		t.Error("it didn't count as a change")
	}
	if grants[0].Gives.Against != AgainstUnreadable {
		t.Errorf("%+v", grants[0])
	}
}

// Addresses are what this report is for, so they're here. Nothing else from those tables is.
func TestTheRolesInTheReportHoldNoSecrets(t *testing.T) {
	_, file := withRole(t)
	rep := stageReport(t, file, filepath.Join(dir(t), "target"), false)
	b, err := json.Marshal(rep.Grants)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range allSecrets {
		if strings.Contains(string(b), secret) {
			t.Errorf("it holds %q", secret)
		}
	}
}
