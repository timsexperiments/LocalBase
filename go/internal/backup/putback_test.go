package backup

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// stageFile stages a file over a new root and says what the report would.
func stageFile(t *testing.T, file string) *Report {
	t.Helper()
	s, err := Stage(context.Background(), file, filepath.Join(dir(t), "new"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Discard)
	return s.Report
}

// A file's own list of its secrets is what it says, not what it holds. The report reads what it holds.
func TestTheReportSaysWhatSecretsTheFileHolds(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	full := create(t, r, false)
	if len(full.Manifest.SecretNames) == 0 {
		t.Fatal("the root holds no secrets, so this tests nothing")
	}
	entries := read(t, full.Output)
	m := manifestOf(t, entries)
	m.SecretNames = nil
	setManifest(t, entries, m)
	pack(t, full.Output, entries)
	rep := stageFile(t, full.Output)
	if !slices.Equal(rep.Secrets, full.Manifest.SecretNames) {
		t.Errorf("the report says it holds %v, and it holds %v", rep.Secrets, full.Manifest.SecretNames)
	}
	if !slices.Equal(rep.SecretsUnlisted, full.Manifest.SecretNames) {
		t.Errorf("left off its own list: %v", rep.SecretsUnlisted)
	}
}

// A file that tells the truth has nothing left off its list.
func TestAFileThatListsItsSecretsHasNothingUnlisted(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	full := create(t, r, false)
	rep := stageFile(t, full.Output)
	if len(rep.Secrets) == 0 || len(rep.SecretsUnlisted) != 0 {
		t.Errorf("secrets %v, unlisted %v", rep.Secrets, rep.SecretsUnlisted)
	}
}

// Nothing a file says is printed as a command. Each one is built here, and comes out the same as the
// command the backup printed.
func TestACommandInTheFileIsNeverUsed(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	bare := create(t, r, true)
	if len(bare.Manifest.SecretsRemoved) < 4 {
		t.Fatalf("too few secrets were left out to test this: %v", bare.Manifest.SecretsRemoved)
	}
	backupSaid := map[string]string{}
	for _, s := range bare.Manifest.SecretsRemoved {
		backupSaid[s.Name] = s.Restore
	}
	entries := read(t, bare.Output)
	m := manifestOf(t, entries)
	for i := range m.SecretsRemoved {
		m.SecretsRemoved[i].Restore = "rm -rf ~"
	}
	setManifest(t, entries, m)
	pack(t, bare.Output, entries)
	rep := stageFile(t, bare.Output)
	if len(rep.PutBack) != len(backupSaid) {
		t.Fatalf("put back: %v", rep.PutBack)
	}
	for _, p := range rep.PutBack {
		if strings.Contains(p.Command, "rm -rf") {
			t.Errorf("%s: the file's command was used: %q", p.Name, p.Command)
		}
		if p.Command == "" || p.Command != backupSaid[p.Name] {
			t.Errorf("%s: restore says %q, and the backup said %q", p.Name, p.Command, backupSaid[p.Name])
		}
	}
}

// A field that doesn't pass the rule of the command it goes into gets no command at all, and neither does a
// kind this version doesn't know, or a file from before these fields.
func TestAFieldThatCantBeCheckedIsPutBackByHand(t *testing.T) {
	good := RemovedSecret{Kind: kindOIDC, ID: "google", Label: "Google", Issuer: "https://accounts.google.com", ClientID: "abc.apps.example.com"}
	for name, r := range map[string]RemovedSecret{
		"a space in an id":            {Kind: kindGitHub, ID: "git hub"},
		"a line break in an id":       {Kind: kindGitHub, ID: "a\nb"},
		"an escape in a name":         {Kind: kindOIDC, ID: "google", Label: "x\x1b[2Jy", Issuer: good.Issuer, ClientID: good.ClientID},
		"an issuer over http":         {Kind: kindOIDC, ID: "google", Label: "Google", Issuer: "http://localhost:8080", ClientID: good.ClientID},
		"an issuer with credentials":  {Kind: kindOIDC, ID: "google", Label: "Google", Issuer: "https://a:b@accounts.google.com", ClientID: good.ClientID},
		"a space in a client id":      {Kind: kindOIDC, ID: "google", Label: "Google", Issuer: good.Issuer, ClientID: "a b"},
		"an origin over http":         {Kind: kindDownload, ID: "vendor", Origins: []string{"http://vendor.example.com"}, Headers: []string{"X-Api-Key"}},
		"an origin with a path":       {Kind: kindDownload, ID: "vendor", Origins: []string{"https://vendor.example.com/x"}, Headers: []string{"X-Api-Key"}},
		"a space in a header name":    {Kind: kindDownload, ID: "vendor", Origins: []string{"https://vendor.example.com"}, Headers: []string{"X Key"}},
		"an unknown kind":             {Kind: "unknown", Name: "something", Restore: "rm -rf ~"},
		"a file from before the kind": {Name: "OIDC client secret (google)", Restore: "local-base access oidc add --id google"},
	} {
		t.Run(name, func(t *testing.T) {
			p := putBack(r)
			if p.Command != "" {
				t.Errorf("it built %q", p.Command)
			}
		})
	}
	if p := putBack(good); p.Command == "" || p.Name != "OIDC client secret (google)" {
		t.Errorf("a good one: %+v", p)
	}
}

// What can be quoted for a shell is, and a value the command takes as it is stays readable.
func TestAValueIsQuotedForAShellOnlyWhenItNeedsIt(t *testing.T) {
	for in, want := range map[string]string{
		"google":                      "google",
		"https://accounts.google.com": "https://accounts.google.com",
		"Acme SSO":                    "'Acme SSO'",
		"it's":                        `'it'\''s'`,
		"$(rm -rf ~)":                 "'$(rm -rf ~)'",
		"":                            "''",
	} {
		if got := shellArg(in); got != want {
			t.Errorf("%q: %s, want %s", in, got, want)
		}
	}
}

// A name from a file is kept short.
func TestANameFromAFileIsBounded(t *testing.T) {
	p := putBack(RemovedSecret{Kind: "unknown", Name: strings.Repeat("x", 5000)})
	if len(p.Name) > 200 {
		t.Errorf("a name of %d bytes", len(p.Name))
	}
}
