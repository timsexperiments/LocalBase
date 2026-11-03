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
		"Everyone who signs in gets: access:manage, keys:manage\n",
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
Everyone who signs in gets: inference:chat
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
Everyone who signs in gets: access:manage, keys:manage (this root gives nothing)
If the access policy is removed, everyone who signs in gets: models:read
Address people sign in at: https://x.example (this root has https://y.example)
Mail server, which can read sign-in links: none (this root has smtp.y.example:587)
`},
		{"what this root has can't be read", backup.SignIn{
			Who:      []backup.Line{{Value: "https://new.example", Named: "oidc:public", Against: backup.AgainstUnreadable}},
			Everyone: backup.Line{Against: backup.AgainstUnreadable},
			Mail:     &backup.Line{Value: "smtp.x.example:587", Against: backup.AgainstUnreadable},
		}, `Who can sign in:
  https://new.example (this file names it oidc:public; this root's can't be read)
Everyone who signs in gets: nothing, unless a role gives it (this root's can't be read)
Mail server, which can read sign-in links: smtp.x.example:587 (this root's can't be read)
`},
		{"no provider at all", backup.SignIn{}, `Sign-in: on this computer only, until a provider is added.
Everyone who signs in gets: nothing, unless a role gives it
`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := signInLines(&c.in); got != c.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, c.want)
			}
		})
	}
}
