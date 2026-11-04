package backup

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"reflect"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/store"
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

// unreadableFact is what a failed read comes to, as a fact to compare. The report has already said it
// couldn't read this. Keys and users may still have been compared, so this can't take them with it, and
// only a failure that may pass, which the caller retries, isn't a fact.
func unreadableFact(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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
	if isBusy(err) {
		return nil, err
	}
	v := readSignIn(ctx, rootDir, db)
	if isBusy(v.policyErr) {
		return nil, v.policyErr
	}
	unreadable := strings.Join([]string{unreadableFact(err), unreadableFact(v.policyErr), unreadableFact(v.accessErr), unreadableFact(v.mailErr)}, ";")
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
	// A -wal that appeared while it waited and isn't this database's would be replayed by opening it, and
	// closing would then write it in, so this stops before that as every other open does.
	if err := store.CheckWALBelongs(dbPath); err != nil {
		return release, refuse("%v", err)
	}
	// What was left mid-write while it waited. A read-write open would undo that here, and the report
	// didn't say so. Write-ahead logging has no such state, and reading this way would leave files.
	if !store.InWAL(dbPath) {
		if err := readableDatabase(ctx, dbPath); isUnfinishedWrite(err) {
			return release, refuse("%s", unfinishedWriteMessage(dir))
		}
	}
	// mode=rw, so that a database deleted while it waited isn't created empty here.
	db, err := sql.Open("sqlite", sqliteDSN(dbPath, fmt.Sprintf("mode=rw&_pragma=busy_timeout(%d)&_pragma=trusted_schema(OFF)", readBusyMillis)))
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
		return stop(refuse("This root's database can't be read now, so what the report said can't be checked. Nothing was changed: %v", err))
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
	if err := db.QueryRowContext(ctx, leaveWAL).Scan(&mode); err != nil {
		if isBusy(err) {
			return stop(refuse("%s", someoneElseHoldsIt))
		}
		return stop(refuse("This root's database can't be changed for the move. Nothing was changed: %v", err))
	}
	// SQLite answers with the mode it stayed in when it can't change it, and this would then move a
	// database still in write-ahead logging.
	if mode != "delete" {
		return stop(refuse("%s", someoneElseHoldsIt))
	}
	if err := take(); err != nil {
		return stop(refuse("%s", someoneElseHoldsIt))
	}
	return held, nil
}

// leaveWAL takes a database out of write-ahead logging. A variable so that a test can see SQLite answer
// with another mode, which nothing found so far makes it do here.
var leaveWAL = `PRAGMA journal_mode=DELETE`

// someoneElseHoldsIt is what a restore says when another process has the database open, or is using it.
const someoneElseHoldsIt = "Something else is using this root's database, so nothing was changed. Stop it or wait, then run this again."
