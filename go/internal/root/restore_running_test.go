package root

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/lease"
)

// A restore that's really running is named as such, with the root quoted. A root's path can hold a control
// character, which mustn't reach a terminal: it could rewrite what's on the screen.
func TestARunningRestoreNamesItsRootSafely(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", base)
	t.Setenv("LOCALBASE_ROOT", "")
	for _, c := range []struct{ name, dir, shown string }{
		{"a plain path", "plain", "plain"},
		{"an escape in it", "erase\x1b[2K", `erase\x1b[2K`},
		{"another of its letters", "café", "café"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(base, c.dir)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, RestoreJournalName), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			lease, err := lease.AcquireOperation(dir, "restore")
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release()
			got := unfinishedRestore(dir)
			if got == nil {
				t.Fatal("no refusal")
			}
			want := "A restore is running on '" + filepath.Join(base, c.shown) + "'. Wait for it to finish, then run this again."
			if got.Error() != want {
				t.Errorf("it says:\n%q\nand should say:\n%q", got.Error(), want)
			}
			if strings.ContainsRune(got.Error(), 0x1b) {
				t.Error("a control character reaches the terminal")
			}
		})
	}
}
