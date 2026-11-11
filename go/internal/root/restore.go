package root

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/timsexperiments/LocalBase/go/internal/lease"
	"github.com/timsexperiments/LocalBase/go/internal/termsafe"
)

// RestoreJournalName is left in a root by a restore that did not finish (DESIGN §34.5).
const RestoreJournalName = ".restore-journal.json"

// unfinishedRestore refuses a root a restore left half done. It is asked separately from
// secure, which asks whether a root can be trusted, and after it, so that a journal someone
// else planted gets that refusal first.
func unfinishedRestore(dir string) error {
	path := filepath.Join(dir, RestoreJournalName)
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	// Telling someone to undo a restore that's still running would be wrong, and the undo would refuse.
	if restoreRunning(dir) {
		return &UnsafeRootError{Problem: "A restore is running on " + shellQuote(termsafe.Printable(dir)) + "." + DefaultRootNote(dir) + " Wait for it to finish, then run this again."}
	}
	return &UnsafeRootError{path, "a restore did not finish", "Run: local-base --root " + shellQuote(dir) + " restore --undo"}
}

// restoreRunning says a live process holds this root's operation lease for a restore. Whatever can't be
// read isn't running: the journal is still there, and the undo takes the lease itself.
func restoreRunning(dir string) bool {
	canonical, err := Canonical(dir)
	if err != nil {
		return false
	}
	return lease.Running(canonical, "restore")
}
