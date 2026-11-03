package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strconv"
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
			"subject someone at " + issuerA},
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
			"subject them at " + issuerA},
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

// A strong role with nobody bound to it isn't harmless if it's the default, so it says so.
func TestTheDefaultRoleIsMarked(t *testing.T) {
	g := grantsIn(t,
		`INSERT INTO auth_roles (id, name, description, created_at, updated_at) VALUES ('member', 'member', 'x', 'x', 'x')`,
		`INSERT INTO auth_settings (id, default_role_id, updated_at) VALUES ('default', 'member', 'x')`)
	if len(g) != 2 || g[0].Role != "member" || !g[0].Default || g[1].Role != "owner" || g[1].Default {
		t.Errorf("%+v", g)
	}
}

// A failed read is an error, and never reads as having no roles.
func TestAFailedReadOfRolesIsAnError(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(dir(t), "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readGrants(context.Background(), db); err == nil {
		t.Error("no error from a database that can't be read")
	}
}

// One table that's there and can't be read is enough, whatever the others say. A table of the wrong
// shape is how that looks, since only this read fails.
func TestAFailedReadOfAnyRoleTableIsAnError(t *testing.T) {
	for _, tables := range [][]string{
		{"auth_roles"}, {"auth_role_permissions"}, {"auth_subject_role_bindings"}, {"auth_email_role_bindings"},
		{"auth_domain_role_bindings"}, {"auth_settings"},
		{"auth_user_roles", "auth_users", "auth_identities"},
		{"auth_user_roles", "auth_users", "auth_identities", "auth_user_emails"},
	} {
		t.Run(strings.Join(tables, "-"), func(t *testing.T) {
			db, err := sql.Open("sqlite", filepath.Join(dir(t), "broken.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			for _, table := range tables {
				if _, err := db.Exec(`CREATE TABLE ` + table + ` (nothing_useful TEXT)`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := readGrants(context.Background(), db); err == nil {
				t.Error("no error from a table that can't be read")
			}
		})
	}
}

// A database with none of these tables can be read, and has no roles. That's a root from before them.
func TestADatabaseWithNoAccessTablesHasNoRoles(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(dir(t), "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	g, err := readGrants(context.Background(), db)
	if err != nil || len(g) != 0 {
		t.Errorf("%+v, %v", g, err)
	}
}

// Each table this reads, gone in turn: a root last opened by an older version isn't unreadable, and
// what's in the other tables is still read.
func TestATableThatIsNotThereHasNoRows(t *testing.T) {
	for _, c := range []struct {
		table string
		rows  []string
		want  string
	}{
		{"go_binding_issuers", []string{`INSERT INTO auth_email_role_bindings (id, role_id, email) VALUES ('b', 'owner', 'a@example.com')`},
			"a@example.com, once a sign-in provider is chosen"},
		{"go_binding_issuers", []string{`INSERT INTO auth_domain_role_bindings (id, role_id, domain) VALUES ('b', 'owner', 'example.com')`},
			"anyone at example.com, once a sign-in provider is chosen"},
		{"go_invitation_issuers", []string{
			`INSERT INTO auth_users (id, status, created_at, updated_at) VALUES ('u', 'pending', 'x', 'x')`,
			`INSERT INTO auth_user_roles (user_id, role_id) VALUES ('u', 'owner')`,
			`INSERT INTO auth_user_emails (user_id, email) VALUES ('u', 'u@example.com')`},
			"u@example.com, invited, once a sign-in provider is chosen"},
		{"auth_subject_role_bindings", nil, ""},
		{"auth_email_role_bindings", nil, ""},
		{"auth_domain_role_bindings", nil, ""},
		{"auth_user_roles", nil, ""},
		{"auth_users", nil, ""},
		{"auth_identities", nil, ""},
		{"auth_user_emails", nil, ""},
		{"auth_settings", nil, ""},
		{"auth_role_permissions", nil, ""},
	} {
		t.Run(c.table+"-"+strconv.Itoa(len(c.want)), func(t *testing.T) {
			rows := append(append([]string{}, c.rows...), `DROP TABLE `+c.table)
			g := grantsIn(t, rows...)
			if len(g) != 1 || g[0].Role != "owner" {
				t.Fatalf("%+v", g)
			}
			if c.want != "" && (len(g[0].Who) != 1 || g[0].Who[0].Value != c.want) {
				t.Errorf("%+v", g[0].Who)
			}
		})
	}
}

func onRootDB(t *testing.T, rootDir string, statements ...string) {
	t.Helper()
	st, err := store.Open(filepath.Join(rootDir, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, stmt := range statements {
		if _, err := st.DB.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

// What a file takes away is as much a change as what it adds. It's how a file locks an owner out, and
// what an honest older backup does to anyone added since.
func TestSomeoneTheFileTakesOffARoleIsSaid(t *testing.T) {
	root, file := withRole(t)
	onRootDB(t, root, `INSERT INTO auth_subject_role_bindings (id, role_id, issuer, subject) VALUES ('b2', 'owner', '`+issuerA+`', 'zz-added-later')`)
	rep := stageReport(t, file, root, true)
	g := rep.Grants[0]
	if len(g.Who) != 2 || g.Who[0].Value != "subject zz-added-later at "+issuerA || g.Who[0].Against != AgainstGone {
		t.Fatalf("%+v", g.Who)
	}
	if g.Who[1].Value != "subject the-owner at "+issuerA || g.Who[1].Against != "" {
		t.Errorf("%+v", g.Who[1])
	}
	if len(rep.RolesGone) != 0 {
		t.Errorf("%+v", rep.RolesGone)
	}
	if !caution(rep, grantsChangedCaution) {
		t.Errorf("no caution: %v", rep.Cautions)
	}
}

// Someone new and someone leaving both go ahead of who stays, or a cap could hide them.
func TestWhoseRoleChangesComesFirst(t *testing.T) {
	root, file := withRole(t)
	bad := editedBackup(t, file, `INSERT INTO auth_subject_role_bindings (id, role_id, issuer, subject) VALUES ('b3', 'owner', '`+issuerA+`', 'zz-new')`)
	onRootDB(t, root, `INSERT INTO auth_subject_role_bindings (id, role_id, issuer, subject) VALUES ('b2', 'owner', '`+issuerA+`', 'zz-leaving')`)
	rep := stageReport(t, bad, root, true)
	who := rep.Grants[0].Who
	if len(who) != 3 || who[2].Value != "subject the-owner at "+issuerA || who[2].Against != "" {
		t.Fatalf("%+v", who)
	}
	if who[0].Against != AgainstNew || who[1].Against != AgainstGone {
		t.Errorf("%+v", who)
	}
}

func TestARoleTheFileDoesNotHaveIsSaid(t *testing.T) {
	root, file := withRole(t)
	onRootDB(t, root,
		`INSERT INTO auth_roles (id, name, description, created_at, updated_at) VALUES ('s', 'support', 'x', 'x', 'x')`,
		`INSERT INTO auth_role_permissions (role_id, permission) VALUES ('s', 'models:read')`,
		`INSERT INTO auth_subject_role_bindings (id, role_id, issuer, subject) VALUES ('b2', 's', '`+issuerA+`', 'helper')`)
	rep := stageReport(t, file, root, true)
	if len(rep.Grants) != 1 || rep.Grants[0].Role != "owner" {
		t.Fatalf("%+v", rep.Grants)
	}
	if len(rep.RolesGone) != 1 {
		t.Fatalf("%+v", rep.RolesGone)
	}
	g := rep.RolesGone[0]
	if g.Role != "support" || g.Gives.Value != "models:read" || len(g.Who) != 1 || g.Who[0].Value != "subject helper at "+issuerA || g.Who[0].Against != AgainstGone {
		t.Errorf("%+v", g)
	}
	if !caution(rep, grantsChangedCaution) {
		t.Errorf("no caution: %v", rep.Cautions)
	}
}

// The role a file drops can be the only one there is.
func TestAFileWithNoRolesOverARootWithOneIsSaid(t *testing.T) {
	root, file := withRole(t)
	bad := editedBackup(t, file, `DELETE FROM auth_subject_role_bindings`, `DELETE FROM auth_role_permissions`, `DELETE FROM auth_settings`, `DELETE FROM auth_roles`)
	rep := stageReport(t, bad, root, true)
	if len(rep.Grants) != 0 || len(rep.RolesGone) != 1 || rep.RolesGone[0].Role != "owner" {
		t.Fatalf("%+v, %+v", rep.Grants, rep.RolesGone)
	}
	if !caution(rep, grantsChangedCaution) {
		t.Errorf("no caution: %v", rep.Cautions)
	}
}

// A new root has nothing to lose, and a root that can't be read can't say what it would.
func TestNothingIsSaidToGoWhenThereIsNothingToCompare(t *testing.T) {
	_, file := withRole(t)
	if rep := stageReport(t, file, filepath.Join(dir(t), "target"), false); len(rep.RolesGone) != 0 {
		t.Errorf("new root: %+v", rep.RolesGone)
	}
	gone, changed := compareGrants([]Grant{{Role: "owner", Who: []Line{}}}, []Grant{{Role: "support", Who: []Line{}}}, context.Canceled)
	if len(gone) != 0 || !changed {
		t.Errorf("unreadable: %+v, %v", gone, changed)
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
	if len(g.Who) != 2 || g.Who[0].Value != "subject zz-added at https://z.example.com" || g.Who[0].Against != AgainstNew {
		t.Fatalf("%+v", g.Who)
	}
	if g.Who[1].Value != "subject the-owner at "+issuerA || g.Who[1].Against != "" {
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
	if _, changed := compareGrants(grants, []Grant{{Role: "owner", Who: []Line{}}}, nil); !changed {
		t.Error("it didn't count as a change")
	}
	if got := grants[0].Gives.Text("gives"); got != "keys:manage (this root gives nothing)" {
		t.Errorf("%q", got)
	}
}

// What this root has can't be read: that's said, and it isn't taken for having none.
func TestRolesThisRootHasBeingUnreadableIsSaid(t *testing.T) {
	grants := []Grant{{Role: "owner", Gives: Line{Value: "keys:manage"}, Who: []Line{{Value: "someone at x"}}}}
	if _, changed := compareGrants(grants, nil, context.Canceled); !changed {
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
