package root

import (
	"os"
	"path/filepath"
	"testing"
)

// A restore that's really running says so, and names the root as it was given, as the sentence for a running
// gateway does. In a message, only a command is quoted.
func TestARunningRestoreNamesItsRoot(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", base)
	t.Setenv("LOCALBASE_ROOT", "")
	dir := filepath.Join(base, "a root")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, RestoreJournalName), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	lease, err := AcquireOperation(dir, "restore")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	got := unfinishedRestore(dir)
	if got == nil {
		t.Fatal("no refusal")
	}
	want := "A restore is running on " + dir + ". Wait for it to finish, then run this again."
	if got.Error() != want {
		t.Errorf("it says:\n%q\nand should say:\n%q", got.Error(), want)
	}
}
