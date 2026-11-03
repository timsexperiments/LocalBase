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

// A place in the other lines the report prints gets the same.
func TestOtherPlacesInTheReportAreShownForWhatTheyAre(t *testing.T) {
	var out bytes.Buffer
	c := &cli.Ctx{Stdout: &out, Stderr: &out}
	printRestoreReport(c, &backup.Report{Manifest: &backup.Manifest{},
		Sources:  []backup.ModelSource{{Model: "m", Kind: "url", Where: "https://\u0435xample.com/a"}},
		Settings: []backup.Setting{{Name: "OTLP endpoint", Value: "https://\u0435xample.com"}},
		SignIn:   &backup.SignIn{Goes: []backup.Line{{Value: "https://\u0435xample.com", Named: "oidc:gone"}}}})
	got := out.String()
	if n := strings.Count(got, `\u0435xample.com`); n != 3 {
		t.Errorf("%d of 3 are escaped:\n%s", n, got)
	}
}
