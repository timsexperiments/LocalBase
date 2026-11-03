package root

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
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
	return &UnsafeRootError{path, "a restore did not finish", "Run: local-base --root " + shellQuote(dir) + " restore --undo"}
}
