package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// crafted makes a backup of a real root, with changes made to one file in it, and returns the backup.
func crafted(t *testing.T, name string, change func(string) string) (root, file string) {
	t.Helper()
	r, _ := newRoot(t, dir(t))
	e := read(t, create(t, r, false).Output)
	found := false
	for i := range e {
		if e[i].header.Name == filesPrefix+name {
			e[i].body, found = []byte(change(string(e[i].body))), true
		}
	}
	if !found {
		t.Fatalf("no %s in the backup", name)
	}
	reseal(t, e)
	file = filepath.Join(dir(t), "crafted"+Extension)
	pack(t, file, e)
	return r.Dir, file
}

func stageReport(t *testing.T, file, target string, replace bool) *Report {
	t.Helper()
	s, err := Stage(context.Background(), file, target, replace)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	t.Cleanup(s.Discard)
	return s.Report
}

func swap(from, to string) func(string) string {
	return func(s string) string {
		if !strings.Contains(s, from) {
			panic(from + " isn't there")
		}
		return strings.ReplaceAll(s, from, to)
	}
}

func hasCaution(rep *Report) bool {
	for _, c := range rep.Cautions {
		if c == signInChangedCaution {
			return true
		}
	}
	return false
}

// A label in the file says nothing about where people really sign in.
func TestTheReportNamesTheRealIssuerNotTheLabel(t *testing.T) {
	_, file := crafted(t, "ui-access.json", swap("https://accounts.example.com", "https://other.example.com"))
	rep := stageReport(t, file, filepath.Join(dir(t), "target"), false)
	if got := rep.SignIn.Who[0]; got.Value != "https://other.example.com" || got.Named != "oidc:google" {
		t.Errorf("%+v", got)
	}
	if got := rep.SignIn.Who[0].Text("has"); got != "https://other.example.com (this file names it oidc:google)" {
		t.Errorf("%q", got)
	}
}

func TestARestoreToANewRootComparesWithNothing(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	rep := stageReport(t, create(t, r, false).Output, filepath.Join(dir(t), "target"), false)
	lines := append(append([]Line{}, rep.SignIn.Who...), rep.SignIn.Everyone, *rep.SignIn.Origin, *rep.SignIn.Mail)
	for _, l := range lines {
		if l.Against != "" || l.ThisRoot != "" {
			t.Errorf("%+v", l)
		}
	}
	if len(rep.SignIn.Goes) != 0 || hasCaution(rep) {
		t.Errorf("goes %v, cautions %v", rep.SignIn.Goes, rep.Cautions)
	}
}

func TestARestoreOfTheSameValuesSaysNothingMore(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	rep := stageReport(t, create(t, r, false).Output, r.Dir, true)
	lines := append(append([]Line{}, rep.SignIn.Who...), rep.SignIn.Everyone, *rep.SignIn.Origin, *rep.SignIn.Mail)
	for _, l := range lines {
		if l.Against != "" || l.ThisRoot != "" {
			t.Errorf("%+v", l)
		}
	}
	if len(rep.SignIn.Goes) != 0 || hasCaution(rep) {
		t.Errorf("goes %v, cautions %v", rep.SignIn.Goes, rep.Cautions)
	}
}

// Each line says what the root has now, and any difference gets the one caution before the question.
func TestEachDifferenceFromThisRootIsSaid(t *testing.T) {
	for _, c := range []struct {
		name, file, from, to string
		line                 func(*SignIn) Line
		thisRoot, text       string
	}{
		{"an issuer", "ui-access.json", "https://accounts.example.com", "https://other.example.com",
			func(s *SignIn) Line { return s.Who[0] }, "https://accounts.example.com",
			"https://other.example.com (this file names it oidc:google; this root has https://accounts.example.com)"},
		{"what everyone gets", "ui-access.json", `"permissions":["inference:chat"]`, `"permissions":["access:manage"]`,
			func(s *SignIn) Line { return s.Everyone }, "inference:chat", "access:manage (this root gives inference:chat)"},
		{"the address", "ui-access.json", "https://localbase.example.com", "https://other.example.com",
			func(s *SignIn) Line { return *s.Origin }, "https://localbase.example.com", "https://other.example.com (this root has https://localbase.example.com)"},
		{"the mail server", "email-delivery.json", "smtp.example.com", "smtp.other.example.com",
			func(s *SignIn) Line { return *s.Mail }, "smtp.example.com:587", "smtp.other.example.com:587 (this root has smtp.example.com:587)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, file := crafted(t, c.file, swap(c.from, c.to))
			rep := stageReport(t, file, root, true)
			l := c.line(rep.SignIn)
			if l.Against != AgainstDiffers || l.ThisRoot != c.thisRoot {
				t.Errorf("%+v", l)
			}
			has := "has"
			if c.name == "what everyone gets" {
				has = "gives"
			}
			if got := l.Text(has); got != c.text {
				t.Errorf("%q, want %q", got, c.text)
			}
			if !hasCaution(rep) {
				t.Errorf("no caution: %v", rep.Cautions)
			}
		})
	}
}

// A provider the root doesn't have, and one it has that the file doesn't.
func TestAProviderComingOrGoingIsSaid(t *testing.T) {
	root, file := crafted(t, "ui-access.json", swap(`"id":"google"`, `"id":"another"`))
	rep := stageReport(t, file, root, true)
	if got := rep.SignIn.Who[0]; got.Against != AgainstNew || got.Text("has") != "https://accounts.example.com (this file names it oidc:another; new to this root)" {
		t.Errorf("%+v", got)
	}
	if len(rep.SignIn.Goes) != 1 || rep.SignIn.Goes[0].Named != "oidc:google" || rep.SignIn.Goes[0].Value != "https://accounts.example.com" {
		t.Errorf("goes: %+v", rep.SignIn.Goes)
	}
	if !hasCaution(rep) {
		t.Errorf("no caution: %v", rep.Cautions)
	}
}

// What the root has can't be read: that's said, and it isn't taken for having none.
func TestWhatThisRootHasBeingUnreadableIsSaid(t *testing.T) {
	for _, c := range []struct {
		name, file string
		lines      func(*SignIn) []Line
	}{
		{"the access file", "ui-access.json", func(s *SignIn) []Line { return []Line{s.Who[0], s.Who[1], s.Everyone, *s.Origin} }},
		{"the mail settings", "email-delivery.json", func(s *SignIn) []Line { return []Line{*s.Mail} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, _ := newRoot(t, dir(t))
			file := create(t, r, false).Output
			if err := os.WriteFile(filepath.Join(r.Dir, c.file), []byte("{ not json"), 0o600); err != nil {
				t.Fatal(err)
			}
			rep := stageReport(t, file, r.Dir, true)
			for _, l := range c.lines(rep.SignIn) {
				if l.Against != AgainstUnreadable || !strings.Contains(l.Text("has"), "this root's can't be read") {
					t.Errorf("%+v", l)
				}
			}
			if !caution(rep, signInUnknownCaution) || caution(rep, signInChangedCaution) {
				t.Errorf("it should say it may change, not that it does: %v", rep.Cautions)
			}
		})
	}
}

// The file has none and the root has one: that's a change too, not a line to leave out.
func TestALineTheFileDoesNotHaveIsStillCompared(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	e := read(t, create(t, r, true).Output)
	kept := e[:0]
	for _, entry := range e {
		if entry.header.Name != filesPrefix+"email-delivery.json" {
			kept = append(kept, entry)
		}
	}
	reseal(t, kept)
	file := filepath.Join(dir(t), "crafted"+Extension)
	pack(t, file, kept)
	rep := stageReport(t, file, r.Dir, true)
	if rep.SignIn.Mail == nil || rep.SignIn.Mail.Value != "none" || rep.SignIn.Mail.ThisRoot != "smtp.example.com:587" {
		t.Errorf("%+v", rep.SignIn.Mail)
	}
}

// What everyone gets depends on whether there's a policy, so each case is said for what it is.
func TestWhatEveryoneGets(t *testing.T) {
	role := func(id, perm string) []string {
		return []string{
			`INSERT INTO auth_roles (id, name, description, created_at, updated_at) VALUES ('` + id + `', '` + id + `', 'x', 'x', 'x')`,
			`INSERT INTO auth_role_permissions (role_id, permission) VALUES ('` + id + `', '` + perm + `')`,
		}
	}
	owner := append(role("owner", "access:manage"), `INSERT INTO auth_subject_role_bindings (id, role_id, issuer, subject) VALUES ('b', 'owner', 'https://accounts.example.com', 's')`)
	for _, c := range []struct {
		name            string
		rows            []string
		access          func(string) string
		everyone, later string
	}{
		{name: "no policy", everyone: "inference:chat"},
		{name: "no policy and nothing in the file", access: swap(`"permissions":["inference:chat"]`, `"permissions":[]`), everyone: ""},
		{name: "a policy with no default role", rows: append(owner, `INSERT INTO auth_settings (id, default_role_id, updated_at) VALUES ('default', NULL, 'x')`), everyone: "", later: "inference:chat"},
		{name: "a policy with a default role", rows: append(append(owner, role("everyone", "models:read")...),
			`INSERT INTO auth_settings (id, default_role_id, updated_at) VALUES ('default', 'everyone', 'x')`), everyone: "models:read", later: "inference:chat"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, _ := newRoot(t, dir(t))
			e := read(t, create(t, r, false).Output)
			if c.access != nil {
				for i := range e {
					if e[i].header.Name == filesPrefix+"ui-access.json" {
						e[i].body = []byte(c.access(string(e[i].body)))
					}
				}
				reseal(t, e)
			}
			if len(c.rows) > 0 {
				editDB(t, e, c.rows...)
			}
			file := filepath.Join(dir(t), "crafted"+Extension)
			pack(t, file, e)
			rep := stageReport(t, file, filepath.Join(dir(t), "target"), false)
			if rep.SignIn.Everyone.Value != c.everyone || rep.SignIn.IfPolicyRemoved != c.later {
				t.Errorf("everyone %q, later %q", rep.SignIn.Everyone.Value, rep.SignIn.IfPolicyRemoved)
			}
		})
	}
}

// A root that gives nobody anything says so in words, so the comparison doesn't read as missing.
func TestThisRootGivingNothingIsSaid(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	file := create(t, r, false).Output
	b, err := os.ReadFile(filepath.Join(r.Dir, "ui-access.json"))
	if err != nil {
		t.Fatal(err)
	}
	emptied := swap(`"permissions":["inference:chat"]`, `"permissions":[]`)(string(b))
	if err := os.WriteFile(filepath.Join(r.Dir, "ui-access.json"), []byte(emptied), 0o600); err != nil {
		t.Fatal(err)
	}
	rep := stageReport(t, file, r.Dir, true)
	if got := rep.SignIn.Everyone.Text("gives"); got != "inference:chat (this root gives nothing)" {
		t.Errorf("%q", got)
	}
}

// A real role can be strong and be the default, with nothing wrong in the database. The report has
// to say so, because anyone at all who signs in would get it.
func TestAStrongDefaultRoleIsSaid(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	e := read(t, create(t, r, false).Output)
	editDB(t, e,
		`INSERT INTO auth_roles (id, name, description, created_at, updated_at) VALUES ('owner', 'owner', 'x', 'x', 'x'), ('member', 'member', 'x', 'x', 'x')`,
		`INSERT INTO auth_role_permissions (role_id, permission) VALUES ('owner', 'access:manage'), ('member', 'access:manage'), ('member', 'keys:manage')`,
		`INSERT INTO auth_subject_role_bindings (id, role_id, issuer, subject) VALUES ('b', 'owner', 'https://accounts.example.com', 's')`,
		`INSERT INTO auth_settings (id, default_role_id, updated_at) VALUES ('default', 'member', 'x')`)
	file := filepath.Join(dir(t), "crafted"+Extension)
	pack(t, file, e)
	rep := stageReport(t, file, filepath.Join(dir(t), "target"), false)
	if got := rep.SignIn.Everyone.Value; got != "access:manage, keys:manage" {
		t.Errorf("%q", got)
	}
}

// Each way of signing in that has no issuer is said in words, and Cloudflare by both of its values.
func TestWaysWithNoIssuerAreSaidInWords(t *testing.T) {
	const access = `{"provider":{"kind":"direct","registrations":[
		{"kind":"github-oauth","id":"gh","name":"GitHub","clientId":"cid","clientSecret":"secret"},
		{"kind":"magic-link","id":"email","name":"Email"}
	]},"origin":"https://localbase.example.com","permissions":[]}`
	_, file := crafted(t, "ui-access.json", func(string) string { return access })
	rep := stageReport(t, file, filepath.Join(dir(t), "target"), false)
	var got []string
	for _, l := range rep.SignIn.Who {
		got = append(got, l.Text("has"))
	}
	want := "GitHub accounts (this file names it github-oauth:gh)|Invited people, with a link sent by email (this file names it magic-link:email)"
	if strings.Join(got, "|") != want {
		t.Errorf("%q", got)
	}

	const cloudflare = `{"provider":{"kind":"cloudflare-access","teamDomain":"team.cloudflareaccess.com","audience":"the-audience"},"origin":"https://localbase.example.com","permissions":[]}`
	_, file = crafted(t, "ui-access.json", func(string) string { return cloudflare })
	rep = stageReport(t, file, filepath.Join(dir(t), "target"), false)
	if len(rep.SignIn.Who) != 1 || rep.SignIn.Who[0].Value != "Cloudflare Access team team.cloudflareaccess.com, audience the-audience" {
		t.Errorf("%+v", rep.SignIn.Who)
	}
}

// Whatever is in those files, the report holds only what decides who can sign in.
func TestTheSignInReportHoldsNoSecrets(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	rep := stageReport(t, create(t, r, false).Output, filepath.Join(dir(t), "target"), false)
	b, err := json.Marshal(rep.SignIn)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{secretOIDC, secretSMTP, `"cid"`, `cid2`, "Google", "Public"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("it holds %q: %s", secret, b)
		}
	}
}

// What everyone gets, read from only the tables that decide it, matches the policy the gateway loads.
func TestWhatEveryoneGetsMatchesThePolicy(t *testing.T) {
	ctx := context.Background()
	role := `INSERT INTO auth_roles (id, name, description, created_at, updated_at) VALUES ('m', 'member', 'x', 'x', 'x')`
	for name, rows := range map[string][]string{
		"no policy":      nil,
		"no default":     {`INSERT INTO auth_settings (id, default_role_id, updated_at) VALUES ('default', NULL, 'x')`},
		"a default":      {role, `INSERT INTO auth_role_permissions (role_id, permission) VALUES ('m', 'models:read'), ('m', 'inference:chat')`, `INSERT INTO auth_settings (id, default_role_id, updated_at) VALUES ('default', 'm', 'x')`},
		"it gives none":  {role, `INSERT INTO auth_settings (id, default_role_id, updated_at) VALUES ('default', 'm', 'x')`},
		"it isn't there": {`PRAGMA foreign_keys=OFF`, `INSERT INTO auth_settings (id, default_role_id, updated_at) VALUES ('default', 'ghost', 'x')`, `INSERT INTO auth_role_permissions (role_id, permission) VALUES ('ghost', 'keys:manage')`},
	} {
		t.Run(name, func(t *testing.T) {
			st, err := store.Open(filepath.Join(dir(t), "local-base.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			st.DB.SetMaxOpenConns(1)
			for _, stmt := range rows {
				if _, err := st.DB.Exec(stmt); err != nil {
					t.Fatalf("%s: %v", stmt, err)
				}
			}
			policy, err := auth.LoadPolicy(ctx, st.DB)
			if err != nil {
				t.Fatal(err)
			}
			var want []string
			if policy != nil && policy.DefaultRole != nil {
				for _, r := range policy.Roles {
					if r.Name == *policy.DefaultRole {
						want = r.Permissions
					}
				}
			}
			has, got, err := readEveryone(ctx, st.DB)
			if err != nil {
				t.Fatal(err)
			}
			if has != (policy != nil) || strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("policy %v, gets %v; the gateway says policy %v, gets %v", has, got, policy != nil, want)
			}
		})
	}
}

// A root last opened by an older version lacks tables added since. What it has is still read.
func TestWhatEveryoneGetsIsReadWithoutTheNewerTables(t *testing.T) {
	st, err := store.Open(filepath.Join(dir(t), "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, stmt := range []string{
		`INSERT INTO auth_roles (id, name, description, created_at, updated_at) VALUES ('m', 'member', 'x', 'x', 'x')`,
		`INSERT INTO auth_role_permissions (role_id, permission) VALUES ('m', 'models:read')`,
		`INSERT INTO auth_settings (id, default_role_id, updated_at) VALUES ('default', 'm', 'x')`,
		`DROP TABLE go_binding_issuers`,
		`DROP TABLE go_invitation_issuers`,
	} {
		if _, err := st.DB.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	has, got, err := readEveryone(context.Background(), st.DB)
	if err != nil || !has || strings.Join(got, ",") != "models:read" {
		t.Errorf("%v, %v, %v", has, got, err)
	}
}

// Without each of the tables that decide it there's no policy, and that isn't a failed read.
func TestWhatEveryoneGetsWithoutTheTablesThatDecideIt(t *testing.T) {
	for _, table := range []string{"auth_settings", "auth_roles", "auth_role_permissions"} {
		t.Run(table, func(t *testing.T) {
			st, err := store.Open(filepath.Join(dir(t), "local-base.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if _, err := st.DB.Exec(`INSERT INTO auth_settings (id, default_role_id, updated_at) VALUES ('default', NULL, 'x')`); err != nil {
				t.Fatal(err)
			}
			st.DB.SetMaxOpenConns(1)
			for _, stmt := range []string{`PRAGMA foreign_keys=OFF`, `DROP TABLE ` + table} {
				if _, err := st.DB.Exec(stmt); err != nil {
					t.Fatalf("%s: %v", stmt, err)
				}
			}
			has, got, err := readEveryone(context.Background(), st.DB)
			if err != nil || has || len(got) != 0 {
				t.Errorf("%v, %v, %v", has, got, err)
			}
		})
	}
}

func TestAFailedReadOfWhatEveryoneGetsIsAnError(t *testing.T) {
	st, err := store.Open(filepath.Join(dir(t), "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readEveryone(context.Background(), st.DB); err == nil {
		t.Error("no error from a database that can't be read")
	}
}
