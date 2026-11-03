package root

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnfinishedRestoreRefusesEveryOpen(t *testing.T) {
	r, err := Init(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(r.Dir); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(r.Dir, RestoreJournalName)
	if err := os.WriteFile(journal, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Open(r.Dir)
	var unsafe *UnsafeRootError
	if !errors.As(err, &unsafe) {
		t.Fatalf("Open: %v", err)
	}
	want := "Refusing to use " + journal + ": a restore did not finish. Run: local-base --root '" + r.Dir + "' restore --undo"
	if err.Error() != want {
		t.Errorf("the message:\n%s\nwant:\n%s", err, want)
	}
	if _, err := Init(r.Dir); err == nil || !strings.Contains(err.Error(), "a restore did not finish") {
		t.Errorf("Init should say the same: %v", err)
	}
	if _, err := OpenForUndo(r.Dir); err != nil {
		t.Errorf("OpenForUndo is how it gets finished: %v", err)
	}
	// Someone else's journal gets the trust refusal, even through the entry that skips the check.
	if err := os.Chmod(journal, 0o666); err != nil {
		t.Fatal(err)
	}
	for name, open := range map[string]func(string) (*Root, error){"Open": Open, "OpenForUndo": OpenForUndo} {
		if _, err := open(r.Dir); err == nil || !strings.Contains(err.Error(), othersCanWrite) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
