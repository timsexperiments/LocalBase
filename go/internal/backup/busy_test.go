package backup

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shortBusyWaits makes a busy database cost milliseconds in these tests. Each read waits a little,
// and a whole step keeps trying for wait.
func shortBusyWaits(t *testing.T, wait time.Duration) {
	t.Helper()
	wasWait, wasRead := busyWait, readBusyMillis
	busyWait, readBusyMillis = wait, 20
	t.Cleanup(func() { busyWait, readBusyMillis = wasWait, wasRead })
}

// holdLock stops every reader of a database, as a writer does while committing. It returns what
// releases it.
func holdLock(t *testing.T, dbPath string) (release func()) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	released := false
	release = func() {
		if !released {
			released = true
			_, _ = db.Exec(`ROLLBACK`)
			_ = db.Close()
		}
	}
	t.Cleanup(release)
	return release
}

// A read that couldn't happen must say so. Each of these used to answer as if it had read an empty
// database, which writes a wrong manifest and prints a wrong warning with no error at all.
func TestABusyDatabaseIsNeverReadAsEmpty(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	holdLock(t, r.DB())
	shortBusyWaits(t, 0)
	ctx := context.Background()
	for name, read := range map[string]func() error{
		"reading the settings":   func() error { _, _, err := configSecrets(ctx, r.DB()); return err },
		"reading the migrations": func() error { _, err := appliedMigrations(ctx, r.DB()); return err },
		"reading the tables":     func() error { _, err := presentRows(ctx, r.DB()); return err },
		"reading the models":     func() error { _, err := selectedModels(ctx, r.DB()); return err },
	} {
		err := read()
		if !isBusy(err) || !strings.HasPrefix(err.Error(), name+":") {
			t.Errorf("%s: %v", name, err)
		}
	}
	target, err := openReadOnly(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if _, err := readComparedKeys(ctx, target); !isBusy(err) {
		t.Errorf("a root's keys: %v", err)
	}
	if _, err := readComparedUsers(ctx, target); !isBusy(err) {
		t.Errorf("a root's users: %v", err)
	}
}

func TestBackupWaitsOutABusyDatabase(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	want := create(t, r, false).Manifest
	release := holdLock(t, r.DB())
	shortBusyWaits(t, 30*time.Second)
	time.AfterFunc(300*time.Millisecond, release)
	res := create(t, r, false)
	copyIsWhole(t, res.Output)
	got := res.Manifest
	if got.Migrations != want.Migrations || strings.Join(got.Models, ",") != strings.Join(want.Models, ",") ||
		strings.Join(got.RowsLeftOut, ",") != strings.Join(want.RowsLeftOut, ",") || strings.Join(got.SecretNames, ",") != strings.Join(want.SecretNames, ",") {
		t.Errorf("after waiting: %+v\nwithout a wait: %+v", got, want)
	}
}

func TestBackupOfADatabaseThatStaysBusySaysSo(t *testing.T) {
	parent := dir(t)
	r, _ := newRoot(t, parent)
	before := tree(t, r.Dir, "runtime")
	holdLock(t, r.DB())
	shortBusyWaits(t, 100*time.Millisecond)
	for _, dry := range []bool{true, false} {
		output := filepath.Join(dir(t), "x"+Extension)
		_, err := Create(context.Background(), Options{Root: r, Output: output, DryRun: dry, ExcludeSecrets: true})
		if err == nil {
			t.Fatal("no error")
		}
		said := err.Error()
		if exists(output) {
			t.Error("a partial backup was left behind")
		}
		retry := "local-base --root '" + r.Dir + "' backup"
		if dry {
			retry += " --dry-run"
		}
		retry += " --exclude-secrets --output '" + output + "'"
		if !strings.Contains(said, retry) {
			t.Errorf("doesn't give the command to run again, %s: %s", retry, said)
		}
		if !strings.Contains(said, "is busy") || !strings.Contains(said, "Try again") || !strings.Contains(said, " stop") {
			t.Errorf("says: %s", said)
		}
		if strings.Contains(said, "database is locked") || strings.Contains(said, "SQLITE_BUSY") {
			t.Errorf("the raw error reaches the person: %s", said)
		}
		if !strings.Contains(said, "reading the settings") {
			t.Errorf("doesn't name its step: %s", said)
		}
	}
	if after := tree(t, r.Dir, "runtime"); after != before {
		t.Errorf("changed the root:\n%s\nwas:\n%s", after, before)
	}
	noLeftovers(t, parent)
}

// A busy root isn't an unreadable one. Reading it as unreadable would skip the comparison, and a
// revoked key would come back, with only a caution to say so.
func TestRestoreOverABusyDatabaseRefuses(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	res := create(t, r, false)
	before := tree(t, r.Dir, "runtime")
	holdLock(t, r.DB())
	shortBusyWaits(t, 100*time.Millisecond)
	_, err := Stage(context.Background(), res.Output, r.Dir, true)
	var refused *Refused
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "is busy") || !strings.Contains(err.Error(), "Try again") {
		t.Fatalf("stage: %v", err)
	}
	if retry := "local-base --root '" + r.Dir + "' restore --replace '" + res.Output + "'"; !strings.Contains(err.Error(), retry) {
		t.Errorf("doesn't give the command to run again, %s: %v", retry, err)
	}
	if strings.Contains(err.Error(), "database is locked") {
		t.Errorf("the raw error reaches the person: %v", err)
	}
	if after := tree(t, r.Dir, "runtime"); after != before {
		t.Errorf("changed the root:\n%s\nwas:\n%s", after, before)
	}
}

func TestRestoreWaitsOutABusyDatabase(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	res := create(t, r, false)
	release := holdLock(t, r.DB())
	shortBusyWaits(t, 30*time.Second)
	time.AfterFunc(300*time.Millisecond, release)
	s, err := Stage(context.Background(), res.Output, r.Dir, true)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	defer s.Discard()
	for _, c := range s.Report.Cautions {
		if strings.Contains(c, "can't be read") {
			t.Errorf("a database that was only busy is reported as unreadable: %s", c)
		}
	}
}

// copyIsWhole opens the database inside a backup: SQLite finds it sound, and it holds the root's key.
func copyIsWhole(t *testing.T, archive string) {
	t.Helper()
	for _, e := range read(t, archive) {
		if e.header.Name != filesPrefix+"local-base.db" {
			continue
		}
		copied := filepath.Join(dir(t), "copied.db")
		if err := os.WriteFile(copied, e.body, 0o600); err != nil {
			t.Fatal(err)
		}
		wholeDatabase(t, copied)
		return
	}
	t.Fatal("the backup holds no database")
}

func wholeDatabase(t *testing.T, path string) {
	t.Helper()
	db, err := openReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var result string
	var keys int
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&result); err != nil || result != "ok" {
		t.Errorf("integrity: %q %v", result, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM api_keys`).Scan(&keys); err != nil || keys == 0 {
		t.Errorf("keys: %d %v", keys, err)
	}
}

// The reads can all pass and a writer still arrive just before the copy, so the copy has its own
// test. A writer that lets go in time costs a wait, and one that doesn't leaves nothing behind.
func TestCopyWaitsOutABusyDatabase(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	release := holdLock(t, r.DB())
	shortBusyWaits(t, 30*time.Second)
	time.AfterFunc(300*time.Millisecond, release)
	stage, to := dir(t), filepath.Join(dir(t), "copy.db")
	if err := copyDatabase(context.Background(), r.DB(), to, stage, false); err != nil {
		t.Fatal(err)
	}
	wholeDatabase(t, to)
}

func TestCopyOfADatabaseThatStaysBusySaysWhere(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	holdLock(t, r.DB())
	shortBusyWaits(t, 100*time.Millisecond)
	stage, to := dir(t), filepath.Join(dir(t), "copy.db")
	err := copyDatabase(context.Background(), r.DB(), to, stage, false)
	if !isBusy(err) || !strings.HasPrefix(err.Error(), "copying the database:") {
		t.Fatalf("copy: %v", err)
	}
	if exists(to) {
		t.Error("a partial copy was left behind")
	}
}

// The first read can pass and a writer arrive just after it. Reading the root's keys or users then
// must not look like finding none, or a revoked key would come back with nothing to say so.
func TestRestoreThatTurnsBusyPartWayRefuses(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	res := create(t, r, false)
	shortBusyWaits(t, 100*time.Millisecond)
	was := targetReadHook
	targetReadHook = func() { holdLock(t, r.DB()) }
	defer func() { targetReadHook = was }()
	_, err := Stage(context.Background(), res.Output, r.Dir, true)
	var refused *Refused
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "is busy") {
		t.Fatalf("stage: %v", err)
	}
}
