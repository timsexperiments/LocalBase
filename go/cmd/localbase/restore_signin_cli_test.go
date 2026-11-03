package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/backup"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
)

// A real backup and the real command: the report says where people really sign in, what everyone who
// does gets, and who can read sign-in links, by the values and not by what the file calls them. No
// secret in those files reaches it.
func TestARestoreReportSaysWhoCanSignIn(t *testing.T) {
	const clientSecret, password, clientID = "planted-client-secret", "planted-smtp-password", "planted-client-id"
	root := noAuthRoot(t)
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("ui-access.json", `{"provider":{"kind":"direct","registrations":[
		{"kind":"oidc","id":"google","name":"A friendly name","issuer":"https://sign-in.example.com","clientId":"`+clientID+`","clientAuthentication":{"kind":"client-secret-basic","clientSecret":"`+clientSecret+`"}}
	]},"origin":"https://localbase.example.com","permissions":["access:manage","keys:manage"]}`)
	write("email-delivery.json", `{"host":"smtp.example.com","port":587,"security":"starttls","authentication":{"kind":"password","username":"planted-username","password":"`+password+`"},"from":"a@example.com"}`)
	file := filepath.Join(t.TempDir(), "b.lbbackup")
	if code, out := runApp(t, "--root", root, "backup", "--output", file); code != 0 {
		t.Fatalf("backup: %s", out)
	}
	code, out := runApp(t, "--root", filepath.Join(t.TempDir(), "target"), "restore", "--dry-run", "--no-port-probe", file)
	if code != 0 {
		t.Fatalf("restore: %s", out)
	}
	for _, want := range []string{
		"Who can sign in:\n  https://sign-in.example.com (this file names it oidc:google)\n",
		"Everyone who signs in through a provider gets: access:manage, keys:manage\n",
		"Address people sign in at: https://localbase.example.com\n",
		"Mail server, which can read sign-in links: smtp.example.com:587\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	code, asJSON := runApp(t, "--json", "--root", filepath.Join(t.TempDir(), "target"), "restore", "--dry-run", "--no-port-probe", file)
	if code != 0 {
		t.Fatalf("restore --json: %s", asJSON)
	}
	if !strings.Contains(asJSON, `"https://sign-in.example.com"`) {
		t.Errorf("--json doesn't carry the issuer: %.400s", asJSON)
	}
	for _, secret := range []string{clientSecret, password, clientID, "planted-username", "A friendly name"} {
		if strings.Contains(out, secret) {
			t.Errorf("the report holds %q", secret)
		}
		if strings.Contains(asJSON, secret) {
			t.Errorf("--json holds %q", secret)
		}
	}
}

func signInLines(s *backup.SignIn) string {
	var out bytes.Buffer
	printSignIn(&cli.Ctx{Stdout: &out, Stderr: &out}, s)
	return out.String()
}

// The words a person reads, for each case.
func TestTheSignInLinesRead(t *testing.T) {
	for _, c := range []struct {
		name string
		in   backup.SignIn
		want string
	}{
		{"nothing to compare", backup.SignIn{
			Who:      []backup.Line{{Value: "https://accounts.example.com", Named: "oidc:google"}, {Value: "GitHub accounts", Named: "github-oauth:gh"}},
			Everyone: backup.Line{Value: "inference:chat"},
			Origin:   &backup.Line{Value: "https://x.example"},
			Mail:     &backup.Line{Value: "smtp.x.example:587"},
		}, `Who can sign in:
  https://accounts.example.com (this file names it oidc:google)
  GitHub accounts (this file names it github-oauth:gh)
Everyone who signs in through a provider gets: inference:chat
Address people sign in at: https://x.example
Mail server, which can read sign-in links: smtp.x.example:587
`},
		{"each difference", backup.SignIn{
			Who: []backup.Line{
				{Value: "https://new.example", Named: "oidc:public", Against: backup.AgainstDiffers, ThisRoot: "https://old.example"},
				{Value: "https://more.example", Named: "oidc:more", Against: backup.AgainstNew},
			},
			Goes:            []backup.Line{{Value: "https://gone.example", Named: "oidc:gone"}},
			Everyone:        backup.Line{Value: "access:manage, keys:manage", Against: backup.AgainstDiffers, ThisRoot: "nothing"},
			IfPolicyRemoved: "models:read",
			Origin:          &backup.Line{Value: "https://x.example", Against: backup.AgainstDiffers, ThisRoot: "https://y.example"},
			Mail:            &backup.Line{Value: "none", Against: backup.AgainstDiffers, ThisRoot: "smtp.y.example:587"},
		}, `Who can sign in:
  https://new.example (this file names it oidc:public; this root has https://old.example)
  https://more.example (this file names it oidc:more; new to this root)
No longer able to sign in: https://gone.example (this root names it oidc:gone)
Everyone who signs in through a provider gets: access:manage, keys:manage (this root gives nothing)
If the access policy is removed, everyone who signs in through a provider gets: models:read
Address people sign in at: https://x.example (this root has https://y.example)
Mail server, which can read sign-in links: none (this root has smtp.y.example:587)
`},
		{"what this root has can't be read", backup.SignIn{
			Who:      []backup.Line{{Value: "https://new.example", Named: "oidc:public", Against: backup.AgainstUnreadable}},
			Everyone: backup.Line{Against: backup.AgainstUnreadable},
			Mail:     &backup.Line{Value: "smtp.x.example:587", Against: backup.AgainstUnreadable},
		}, `Who can sign in:
  https://new.example (this file names it oidc:public; this root's can't be read)
Everyone who signs in through a provider gets: nothing, unless a role gives it (this root's can't be read)
Mail server, which can read sign-in links: smtp.x.example:587 (this root's can't be read)
`},
		{"no provider at all", backup.SignIn{}, `Sign-in: on this computer only, until a provider is added.
Everyone who signs in through a provider gets: nothing, unless a role gives it
`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := signInLines(&c.in); got != c.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, c.want)
			}
		})
	}
}

func grantLines(grants []backup.Grant) string {
	var out bytes.Buffer
	printGrants(&cli.Ctx{Stdout: &out, Stderr: &out}, grants, nil)
	return out.String()
}

// What would go, in the words a person reads, ahead of what stays.
func TestTheLinesForWhatGoesRead(t *testing.T) {
	var out bytes.Buffer
	printGrants(&cli.Ctx{Stdout: &out, Stderr: &out},
		[]backup.Grant{{Role: "owner", Gives: backup.Line{Value: "keys:manage"}, Who: []backup.Line{
			{Value: "subject added at https://x.example", Against: backup.AgainstNew},
			{Value: "subject leaving at https://x.example", Against: backup.AgainstGone},
			{Value: "subject stays at https://x.example"}}}},
		[]backup.Grant{
			{Role: "support", Gives: backup.Line{Value: "models:read"}, Who: []backup.Line{{Value: "subject helper at https://x.example", Against: backup.AgainstGone}}},
			{Role: "unused", Who: []backup.Line{}}})
	want := `What each role gives, and who has it:
  No longer a role here: support (this root gives models:read)
    no longer has this role: subject helper at https://x.example
  No longer a role here: unused (this root gives nothing)
  owner: keys:manage
    subject added at https://x.example (new to this root)
    no longer has this role: subject leaving at https://x.example
    subject stays at https://x.example
`
	if got := out.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// With no role left in the file, the section is still there, since it's all about what goes.
func TestTheRoleSectionPrintsWhenOnlyRolesGo(t *testing.T) {
	var out bytes.Buffer
	printGrants(&cli.Ctx{Stdout: &out, Stderr: &out}, nil, []backup.Grant{{Role: "owner", Who: []backup.Line{}}})
	if !strings.HasPrefix(out.String(), "What each role gives, and who has it:\n  No longer a role here: owner") {
		t.Errorf("%s", out.String())
	}
}

// A cap can't hide anyone losing a role without saying so, on a role that stays or one that goes.
func TestALongListSaysHowManyAreLosingARole(t *testing.T) {
	who := func() []backup.Line {
		var lines []backup.Line
		for range maxGrantLines {
			lines = append(lines, backup.Line{Value: "someone", Against: backup.AgainstGone})
		}
		return append(lines, backup.Line{Value: "late", Against: backup.AgainstNew}, backup.Line{Value: "later", Against: backup.AgainstGone}, backup.Line{Value: "last", Against: backup.AgainstGone})
	}
	const more = "    and 3 more, 1 of them new, 2 of them losing this role. All of them: restore --dry-run --json\n"
	var out bytes.Buffer
	c := &cli.Ctx{Stdout: &out, Stderr: &out}
	printGrants(c, []backup.Grant{{Role: "owner", Who: who()}}, nil)
	if !strings.HasSuffix(out.String(), more) {
		t.Errorf("on a role that stays: %s", out.String()[max(0, out.Len()-200):])
	}
	out.Reset()
	printGrants(c, nil, []backup.Grant{{Role: "owner", Who: who()}})
	if !strings.HasSuffix(out.String(), more) {
		t.Errorf("on a role that goes: %s", out.String()[max(0, out.Len()-200):])
	}
}

func TestTheRoleLinesRead(t *testing.T) {
	got := grantLines([]backup.Grant{
		{Role: "chat", Gives: backup.Line{Value: "inference:chat"}, Who: []backup.Line{}},
		{Role: "empty", Who: []backup.Line{{Value: "subject 1234 at https://x.example"}}},
		{Role: "member", Gives: backup.Line{Value: "models:read"}, Default: true, Who: []backup.Line{}},
		{Role: "owner", Gives: backup.Line{Value: "access:manage, keys:manage", Against: backup.AgainstDiffers, ThisRoot: "access:manage"},
			Who: []backup.Line{{Value: "subject 1234 at https://x.example", Against: backup.AgainstNew}, {Value: "a@example.com, signing in through https://x.example"}}},
		{Role: "shared", Gives: backup.Line{Value: "models:read"}, Default: true, Who: []backup.Line{{Value: "b@example.com, once a sign-in provider is chosen"}}},
	})
	want := `What each role gives, and who has it:
  chat: inference:chat
    nobody
  empty: nothing
    subject 1234 at https://x.example
  member: models:read (everyone who signs in through a provider has this)
    nobody else
  owner: access:manage, keys:manage (this root gives access:manage)
    subject 1234 at https://x.example (new to this root)
    a@example.com, signing in through https://x.example
  shared: models:read (everyone who signs in through a provider has this)
    b@example.com, once a sign-in provider is chosen
`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if grantLines(nil) != "" {
		t.Error("it printed a heading with no roles")
	}
}

// A long list says how many it didn't print, and how many of those are new, and where to see them.
func TestALongRoleSaysHowManyMore(t *testing.T) {
	who := []backup.Line{{Value: "added", Against: backup.AgainstNew}}
	for range maxGrantLines + 9 {
		who = append(who, backup.Line{Value: "someone"})
	}
	who = append(who, backup.Line{Value: "late", Against: backup.AgainstNew}, backup.Line{Value: "later", Against: backup.AgainstNew})
	got := grantLines([]backup.Grant{{Role: "owner", Gives: backup.Line{Value: "keys:manage"}, Who: who}})
	if !strings.Contains(got, "    added (new to this root)\n") {
		t.Errorf("%s", got)
	}
	if !strings.HasSuffix(got, "    and 12 more, 2 of them new. All of them: restore --dry-run --json\n") {
		t.Errorf("%s", got[max(0, len(got)-200):])
	}
	if n := strings.Count(got, "\n    "); n != maxGrantLines+1 {
		t.Errorf("%d lines", n)
	}
	got = grantLines([]backup.Grant{{Role: "owner", Who: who[1 : maxGrantLines+2]}})
	if !strings.HasSuffix(got, "    and 1 more. All of them: restore --dry-run --json\n") {
		t.Errorf("%s", got[max(0, len(got)-200):])
	}
}

// Who has a role is judged by eye too, so a lookalike in it is shown for what it is, and this goes
// through the same copy as the rest of the report.
func TestALookalikeInWhoHasARoleIsShownForWhatItIs(t *testing.T) {
	var out bytes.Buffer
	printRestoreReport(&cli.Ctx{Stdout: &out, Stderr: &out}, &backup.Report{Manifest: &backup.Manifest{},
		Grants: []backup.Grant{{Role: "owner", Gives: backup.Line{Value: "keys:manage"},
			Who: []backup.Line{{Value: "subject 1 at https://\u0430ccounts.example.com"}}}}})
	if got := out.String(); !strings.Contains(got, `subject 1 at https://\u0430ccounts.example.com`) {
		t.Errorf("%s", got)
	}
}
