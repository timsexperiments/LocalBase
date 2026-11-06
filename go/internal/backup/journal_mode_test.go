package backup

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/root"
)

// journalOf reads what a database's header says, without opening it, since opening can change it.
func journalOf(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 20 {
		t.Fatalf("%d bytes isn't a database", len(b))
	}
	switch {
	case b[18] == 2 && b[19] == 2:
		return "wal"
	case b[18] == 1 && b[19] == 1:
		return "rollback"
	}
	return "unknown"
}

// A backup whose database says it's in write-ahead mode.
func claimingWAL(t *testing.T, file string) string {
	t.Helper()
	e := read(t, file)
	editDB(t, e, `PRAGMA journal_mode=WAL`)
	for i := range e {
		if e[i].header.Name != filesPrefix+"local-base.db" {
			continue
		}
		claim := filepath.Join(dir(t), "claim.db")
		if err := os.WriteFile(claim, e[i].body, 0o600); err != nil {
			t.Fatal(err)
		}
		if got := journalOf(t, claim); got != "wal" {
			t.Fatalf("the test's file says %s, so this would prove nothing", got)
		}
	}
	out := filepath.Join(dir(t), "claiming"+Extension)
	pack(t, out, e)
	return out
}

func beside(t *testing.T, path string) []string {
	t.Helper()
	var found []string
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(path + suffix); err == nil {
			found = append(found, suffix)
		}
	}
	return found
}

// How a database is journaled is the store's to decide, so a file's claim doesn't carry over: one claiming
// write-ahead logging ends up the same as an honest one, whichever mode the store uses. Nothing is left
// beside it, and what it holds is all there. The header is read right after Commit, and not opened.
func TestAFilesClaimedJournalModeDoesNotCarryOver(t *testing.T) {
	restored := func(t *testing.T, file, where string, replace bool) (mode string, extra []string) {
		t.Helper()
		s, err := Stage(context.Background(), file, where, replace)
		if err != nil {
			t.Fatalf("stage: %v", err)
		}
		if _, err := s.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		db := filepath.Join(where, "local-base.db")
		return journalOf(t, db), beside(t, db)
	}
	for name, target := range map[string]func(*keyRoot) (string, bool){
		"on a new root":    func(*keyRoot) (string, bool) { return filepath.Join(dir(t), "target"), false },
		"replacing a root": func(k *keyRoot) (string, bool) { return k.r.Dir, true },
	} {
		t.Run(name, func(t *testing.T) {
			honest := withKey(t)
			whereHonest, replace := target(honest)
			wantMode, wantExtra := restored(t, honest.file, whereHonest, replace)

			k := withKey(t)
			bad := claimingWAL(t, k.file)
			where, replace := target(k)
			mode, extra := restored(t, bad, where, replace)
			if mode != wantMode {
				t.Errorf("a file claiming write-ahead logging ended up %s, and an honest one %s", mode, wantMode)
			}
			if strings.Join(extra, ",") != strings.Join(wantExtra, ",") {
				t.Errorf("beside it: %v, and beside an honest one: %v", extra, wantExtra)
			}
			if got := (&keyRoot{r: &root.Root{Dir: where}}); !got.works(t, k.secret) {
				t.Error("the key in it doesn't work")
			}
		})
	}
}

// If it can't be done, a restore doesn't go on as if it had been. Another connection in the middle of a
// read is how that looks.
func TestAModeThatWontChangeIsSaid(t *testing.T) {
	path := filepath.Join(dir(t), "held.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(50)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{`PRAGMA journal_mode=WAL`, `CREATE TABLE t (v TEXT)`, `INSERT INTO t VALUES ('x')`} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	other, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(50)")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	tx, err := other.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var v string
	if err := tx.QueryRow(`SELECT v FROM t`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if err := settleJournalMode(context.Background(), db); err == nil {
		t.Error("it said nothing about a mode that didn't change")
	}
}

// SQLite can answer with the mode it stayed in, without an error. That's a refusal too.
func TestAModeThatStaysIsAnError(t *testing.T) {
	db, err := sql.Open("sqlite", "file:settle?mode=memory&cache=private")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	err = settleJournalMode(context.Background(), db)
	if err == nil || err.Error() != "its journal mode stayed memory" {
		t.Errorf("a database that stays in memory mode: %v", err)
	}
}

// When it can't be done, the restore stops there, says why, and leaves nothing behind.
func TestARestoreThatCantSettleTheModeRefuses(t *testing.T) {
	k := withKey(t) // made first: making a backup checks that it would restore
	parent := dir(t)
	was := settleJournal
	defer func() { settleJournal = was }()
	settleJournal = func(context.Context, *sql.DB) error { return errors.New("its journal mode stayed wal") }
	_, err := Stage(context.Background(), k.file, filepath.Join(parent, "target"), false)
	var refused *Refused
	if !errors.As(err, &refused) || err.Error() != "The database can't be used: its journal mode stayed wal" {
		t.Fatalf("stage: %v", err)
	}
	noLeftovers(t, parent)
}

// A connection that's closed is an error too, not a mode.
func TestAFailedChangeOfModeIsAnError(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(dir(t), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := settleJournalMode(context.Background(), db); err == nil {
		t.Error("no error from a database that can't be used")
	}
}
