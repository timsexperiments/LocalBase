package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/acquire"
)

// withProfiles is a backup whose profile file says body, as one written by hand, or crafted, could.
func withProfiles(t *testing.T, body string) string {
	t.Helper()
	r, _ := newRoot(t, dir(t))
	e := read(t, create(t, r, false).Output)
	for i := range e {
		if e[i].header.Name == filesPrefix+"credentials/auth-profiles.json" {
			e[i].body = []byte(body)
			reseal(t, e)
			out := filepath.Join(dir(t), "crafted"+Extension)
			pack(t, out, e)
			return out
		}
	}
	t.Fatal("no profile file in the backup")
	return ""
}

// A backup holding a profile that fails the checks every use of it makes is refused, saying which and why, and
// nothing is left behind.
func TestABackupWithAProfileThatFailsItsChecksIsRefused(t *testing.T) {
	file := withProfiles(t, `{"profiles":[{"id":"vendor","origins":["https://vendor.example.com"],"headers":{"Host":"`+secretHeader+`"}}]}`)
	parent := dir(t)
	_, err := Stage(context.Background(), file, filepath.Join(parent, "target"), false)
	want := `The download credentials in this backup are not valid: download profile "vendor" is not valid: header Host is controlled by the downloader`
	var refused *Refused
	if !errors.As(err, &refused) || err.Error() != want {
		t.Fatalf("got %v", err)
	}
	noLeftovers(t, parent)
}

// A backup whose huggingface profile names origins that aren't Hugging Face's own restores, as a root with that
// file still works: those origins are left out wherever the profile is used, with a warning, and the rest of it
// works.
func TestABackupWhoseHuggingFaceProfileNamesOtherOriginsRestores(t *testing.T) {
	t.Setenv("HF_ENDPOINT", "")
	for _, c := range []struct {
		name    string
		origins string
		inUse   []string
	}{
		{"some of them", `"https://cas-bridge.xethub.hf.co","https://evil.example"`, []string{"https://cas-bridge.xethub.hf.co"}},
		{"all of them", `"https://evil.example"`, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := `{"profiles":[{"id":"huggingface","origins":[` + c.origins + `]}]}`
			file := withProfiles(t, body)
			target := filepath.Join(dir(t), "target")
			restore(t, file, target, false)
			got, err := os.ReadFile(filepath.Join(target, "credentials", "auth-profiles.json"))
			if err != nil || string(got) != body {
				t.Fatalf("the restored file: %q %v", got, err)
			}
			ps, err := acquire.LoadProfiles(target)
			if err != nil {
				t.Fatal(err)
			}
			var inUse []string
			if p := ps.Get(acquire.HFProfileID); p != nil {
				inUse = p.Origins
			}
			if !slices.Equal(inUse, c.inUse) || len(ps.Warnings) != 1 || ps.FirstInvalid() != nil {
				t.Errorf("in use: %q, warnings %q, invalid %v", inUse, ps.Warnings, ps.FirstInvalid())
			}
		})
	}
}
