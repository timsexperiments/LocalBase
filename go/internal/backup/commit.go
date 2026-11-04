package backup

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"reflect"
	"strings"
)

// liveFacts is everything about the root being replaced that a report tells someone. They say yes to
// that, so a restore reads it twice: before the report, and again after the answer. It is read the same
// way both times, from any database handle.
type liveFacts struct {
	keys   map[string]comparedKey
	users  map[string]comparedUser
	grants []Grant
	signIn signInFacts
}

// signInFacts is the part of signInValues that can be compared.
type signInFacts struct {
	ways                []Line
	everyone, ifRemoved []string
	origin, mail        string
	unreadable          string
}

func readLiveFacts(ctx context.Context, db *sql.DB, rootDir string) (*liveFacts, error) {
	keys, err := readComparedKeys(ctx, db)
	if err != nil {
		return nil, err
	}
	users, err := readComparedUsers(ctx, db)
	if err != nil {
		return nil, err
	}
	grants, err := readGrants(ctx, db)
	if err != nil {
		return nil, err
	}
	v := readSignIn(ctx, rootDir, db)
	if v.policyErr != nil {
		return nil, v.policyErr
	}
	// A file that can't be read is a fact to compare: it decides who can sign in, and it may be why this
	// root is being replaced.
	unreadable := ""
	for _, err := range []error{v.accessErr, v.mailErr} {
		if err != nil {
			unreadable += err.Error() + ";"
		}
	}
	return &liveFacts{keys, users, grants, signInFacts{v.ways, v.everyone, v.ifRemoved, v.origin, v.mail, unreadable}}, nil
}

// changedFrom says in words what differs. A difference of any kind means the report someone answered
// no longer holds, so this says what kind, and leaves the detail to the report they'll see next.
func (f *liveFacts) changedFrom(now *liveFacts) []string {
	var changed []string
	if !maps.Equal(f.keys, now.keys) {
		changed = append(changed, "API keys")
	}
	if !maps.Equal(f.users, now.users) {
		changed = append(changed, "users")
	}
	if !reflect.DeepEqual(f.grants, now.grants) {
		changed = append(changed, "roles")
	}
	if !reflect.DeepEqual(f.signIn, now.signIn) {
		changed = append(changed, "sign-in settings")
	}
	return changed
}

// readRootFacts reads the root being replaced, for a report. A database that's only busy says so, and a
// failure of any other kind comes back as it is, for the caller: such a root has nothing to hold it to.
func readRootFacts(ctx context.Context, dbPath, rootDir string) (*liveFacts, error) {
	var facts *liveFacts
	err := retryBusy(ctx, func() error {
		db, err := openReadOnly(dbPath)
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }() // Read-only.
		facts, err = readLiveFacts(ctx, db, rootDir)
		return err
	})
	return facts, err
}

// holdRoot takes the root's database for this process and compares it with what the report said. A
// transaction can't span the move, which is a rename of a separate file, so the lock is what does: from
// the comparison until the database has moved, a command already past root.Open can't write. Its write
// waits, and then fails, because SQLite sees the file has moved, so nothing is lost without anyone being
// told. It needs nothing from the commands themselves. Commands that start now are stopped earlier, by
// the journal. The caller has written that, and calls release once the database has moved.
//
// It runs in both journal modes. locking_mode=EXCLUSIVE keeps the file as it is, for a clean root that
// stops here. journal_mode=DELETE then takes it out of write-ahead logging for the move, and does nothing
// to a file already out of it: without that, a process that had opened the file but not read it would
// wait out the lock and then write a -wal beside the restored database, which reads as that process's
// rows and passes integrity_check.
//
// With nothing to compare, as on a new root, or one that couldn't be read for the report, there's
// nothing to hold, and that file is never opened for writing: opening a broken one could change it, or
// replay a -wal that isn't its own.
func (s *Staged) holdRoot(ctx context.Context, dir, dbPath string) (release func(), err error) {
	release = func() {}
	if err := stepHook("hold"); err != nil {
		return release, err
	}
	if s.live == nil {
		return release, nil
	}
	// What was left mid-write while it waited. A read-write open would undo that here, and the report
	// didn't say so.
	if err := readableDatabase(ctx, dbPath); isUnfinishedWrite(err) {
		return release, refuse("%s", unfinishedWriteMessage(dir))
	}
	db, err := sql.Open("sqlite", sqliteDSN(dbPath, fmt.Sprintf("_pragma=busy_timeout(%d)&_pragma=trusted_schema(OFF)", readBusyMillis)))
	if err != nil {
		return release, refuse("This root's database can't be opened: %v", err)
	}
	db.SetMaxOpenConns(1)
	held := func() {
		_, _ = db.ExecContext(ctx, `ROLLBACK`) // Nothing was written, and closing ends it anyway.
		_ = db.Close()                         // The same: there's nothing a failed close could lose.
	}
	stop := func(err error) (func(), error) {
		held()
		return func() {}, err
	}
	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA locking_mode=EXCLUSIVE`).Scan(&mode); err != nil || mode != "exclusive" {
		return stop(refuse("This root's database can't be locked (%q): %v", mode, err))
	}
	take := func() error {
		_, err := db.ExecContext(ctx, `BEGIN EXCLUSIVE`)
		return err
	}
	if err := retryBusy(ctx, take); isBusy(err) {
		return stop(refuse("%s", someoneElseHoldsIt))
	} else if err != nil {
		return stop(refuse("This root's database can't be read now, so what the report said can't be checked. Nothing was changed: %v", err))
	}
	now, err := readLiveFacts(ctx, db, dir)
	if err != nil {
		return stop(refuse("This root's database can't be read now, so what the report said can't be checked. Nothing was changed: %v", err))
	}
	if err := stepHook("compared"); err != nil {
		return stop(err)
	}
	if changed := s.live.changedFrom(now); len(changed) > 0 {
		return stop(refuse("This root changed while this waited: %s. What the report said no longer holds, so nothing was changed. Run this again to see what it would do now.", strings.Join(changed, ", ")))
	}
	if _, err := db.ExecContext(ctx, `ROLLBACK`); err != nil {
		return stop(refuse("This root's database can't be read now, so what the report said can't be checked. Nothing was changed: %v", err))
	}
	// The lock stays: that's what locking_mode=EXCLUSIVE means, so nothing gets in between these.
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode=DELETE`).Scan(&mode); err != nil {
		if isBusy(err) {
			return stop(refuse("%s", someoneElseHoldsIt))
		}
		return stop(refuse("This root's database can't be changed for the move. Nothing was changed: %v", err))
	}
	if err := take(); err != nil {
		return stop(refuse("%s", someoneElseHoldsIt))
	}
	return held, nil
}

// someoneElseHoldsIt is what a restore says when another process has the database open, or is using it.
const someoneElseHoldsIt = "Something else is using this root's database, so nothing was changed. Stop it or wait, then run this again."
