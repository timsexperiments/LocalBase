package store

import (
	"errors"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// BusyError is a database that stayed locked past SQLite's wait: the gateway or another command is
// writing to it. Its message is what a person should read, and the raw error stays underneath.
type BusyError struct{ Err error }

func (e *BusyError) Error() string {
	return "The database is busy: the gateway or another command is writing to it. Try again in a moment."
}

func (e *BusyError) Unwrap() error { return e.Err }

// IsBusy says whether a failure was only the database being locked by someone else, so that a
// caller can say so, rather than treating it as a broken or empty database.
func IsBusy(err error) bool {
	var e *sqlite.Error
	if !errors.As(err, &e) {
		return false
	}
	code := e.Code() & 0xff
	return code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED
}
