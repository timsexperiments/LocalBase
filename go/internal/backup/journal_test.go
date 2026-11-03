package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// crashMidWrite leaves a database as a process killed during a large write would: pages changed
// on disk, and a journal that undoes them. It writes to a copy and takes that copy's files while
// the transaction is open, because a test can't kill itself. Those files go to dbPath.
func crashMidWrite(t *testing.T, dbPath string) {
	t.Helper()
	work := filepath.Join(dir(t), "copy.db")
	body, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(work, body, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+work+"?_pragma=cache_size(8)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`CREATE TABLE crash_mid_write (x)`); err != nil {
		t.Fatal(err)
	}
	for range 64 {
		if _, err := tx.Exec(`INSERT INTO crash_mid_write VALUES (randomblob(65536))`); err != nil {
			t.Fatal(err)
		}
	}
	for from, to := range map[string]string{work: dbPath, work + "-journal": dbPath + "-journal"} {
		b, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) == 0 {
			t.Fatalf("%s is empty, so this is not a crash", from)
		}
		if err := os.WriteFile(to, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// recovers says a database and its journal are still a working pair: an ordinary open undoes the
// write that never finished, and what was there before it is intact.
func recovers(t *testing.T, dbPath string, keys int) {
	t.Helper()
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("opening it: %v", err)
	}
	defer st.Close()
	var result string
	if err := st.DB.QueryRow(`PRAGMA integrity_check`).Scan(&result); err != nil || result != "ok" {
		t.Fatalf("integrity: %q %v", result, err)
	}
	var left, got int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'crash_mid_write'`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("the unfinished write is still there: %d %v", left, err)
	}
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM api_keys`).Scan(&got); err != nil || got != keys {
		t.Fatalf("keys: %d, want %d: %v", got, keys, err)
	}
}

func keyCount(t *testing.T, dbPath string) int {
	t.Helper()
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var n int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM api_keys`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The helper has to make what it says it makes, or every test here proves nothing.
func TestCrashMidWriteIsARealCrash(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	dbPath := filepath.Join(r.Dir, "local-base.db")
	keys := keyCount(t, dbPath)
	crashMidWrite(t, dbPath)
	if !exists(dbPath + "-journal") {
		t.Fatal("no journal")
	}
	err := readableDatabase(context.Background(), dbPath)
	if !isUnfinishedWrite(err) {
		t.Fatalf("a read-only open: %v", err)
	}
	recovers(t, dbPath, keys)
	if exists(dbPath + "-journal") {
		t.Error("an ordinary open should have finished with the journal")
	}
}

func TestBackupOfACrashedRootSaysWhatToDo(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	dbPath := filepath.Join(r.Dir, "local-base.db")
	keys := keyCount(t, dbPath)
	crashMidWrite(t, dbPath)
	before := tree(t, r.Dir, "runtime")
	for _, dry := range []bool{true, false} {
		_, err := Create(context.Background(), Options{Root: r, Output: filepath.Join(dir(t), "x"+Extension), DryRun: dry})
		if err == nil || !strings.Contains(err.Error(), "never finished") || !strings.Contains(err.Error(), " status") {
			t.Fatalf("dry run %v: %v", dry, err)
		}
	}
	if after := tree(t, r.Dir, "runtime"); after != before {
		t.Errorf("a refused backup changed the root:\n%s\nwas:\n%s", after, before)
	}
	recovers(t, dbPath, keys)
	create(t, r, false)
}

func TestRestoreOverACrashedRootSaysWhatToDo(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	res := create(t, r, false)
	dbPath := filepath.Join(r.Dir, "local-base.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`UPDATE api_keys SET revoked_at = '2026-11-02T00:00:00.000Z' WHERE revoked_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	st.Close()
	keys := keyCount(t, dbPath)
	crashMidWrite(t, dbPath)
	before := tree(t, r.Dir, "runtime")
	_, err = Stage(context.Background(), res.Output, r.Dir, true)
	var refused *Refused
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "never finished") || !strings.Contains(err.Error(), " status") {
		t.Fatalf("stage: %v", err)
	}
	if after := tree(t, r.Dir, "runtime"); after != before {
		t.Errorf("a refused restore changed the root:\n%s\nwas:\n%s", after, before)
	}
	// Once it is recovered, the revocation it held is seen, and stays.
	recovers(t, dbPath, keys)
	s, err := Stage(context.Background(), res.Output, r.Dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Discard()
	if s.Report.RevocationsKept == 0 || len(s.Report.KeysLeftOut) != 0 {
		t.Errorf("after recovering: kept %d, left out %v", s.Report.RevocationsKept, s.Report.KeysLeftOut)
	}
}

// A crash can also come after the checks, while the person reads the report: the journal has to go
// wherever its database goes. Left behind, SQLite plays it into the restored database and uses it
// up, so that neither database survives.
func TestRestoreKeepsAJournalWithItsDatabase(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	res := create(t, r, false)
	dbPath := filepath.Join(r.Dir, "local-base.db")
	keys := keyCount(t, dbPath)
	s, err := Stage(context.Background(), res.Output, r.Dir, true)
	if err != nil {
		t.Fatal(err)
	}
	crashMidWrite(t, dbPath)
	done, err := s.Commit()
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if exists(dbPath + "-journal") {
		t.Error("the old journal is beside the restored database")
	}
	recovers(t, dbPath, keys)
	recovers(t, filepath.Join(done.BeforeDir, "local-base.db"), keys)
}

func TestRestoreFailureAndCrashBringBackDatabaseAndJournal(t *testing.T) {
	setup := func(t *testing.T) (*root.Root, *Staged, string, int) {
		t.Helper()
		r, _ := newRoot(t, dir(t))
		res := create(t, r, false)
		dbPath := filepath.Join(r.Dir, "local-base.db")
		keys := keyCount(t, dbPath)
		s, err := Stage(context.Background(), res.Output, r.Dir, true)
		if err != nil {
			t.Fatal(err)
		}
		crashMidWrite(t, dbPath)
		return r, s, dbPath, keys
	}
	var all []string
	func() {
		_, s, _, _ := setup(t)
		was := stepHook
		stepHook = func(step string) error { all = append(all, step); return nil }
		defer func() { stepHook = was }()
		if _, err := s.Commit(); err != nil {
			t.Fatal(err)
		}
	}()
	if !strings.Contains(strings.Join(all, ","), "aside local-base.db-journal") {
		t.Fatalf("the journal was not moved: %v", all)
	}
	for _, name := range all {
		for _, crash := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s crash=%v", name, crash), func(t *testing.T) {
				r, s, dbPath, keys := setup(t)
				was := stepHook
				stepHook = func(step string) error {
					if step != name {
						return nil
					}
					if crash {
						panic("crash")
					}
					return fmt.Errorf("injected at %s", step)
				}
				defer func() { stepHook = was }()
				if crash {
					func() {
						defer func() {
							if recover() == nil {
								t.Fatal("no crash")
							}
						}()
						_, _ = s.Commit()
					}()
					stepHook = was
					if err := Undo(r.Dir); err != nil {
						t.Fatal(err)
					}
				} else if _, err := s.Commit(); err == nil || !strings.Contains(err.Error(), "put back") {
					t.Fatalf("commit: %v", err)
				}
				recovers(t, dbPath, keys)
			})
		}
	}
}

// A root is often replaced because its database is broken, so this can't refuse. But "nothing is
// revoked" and "this couldn't be read" must not look the same.
func TestRestoreOverAnUnreadableDatabaseSaysSo(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	res := create(t, r, false)
	dbPath := filepath.Join(r.Dir, "local-base.db")
	if err := os.WriteFile(dbPath, []byte(strings.Repeat("not a database ", 400)), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Stage(context.Background(), res.Output, r.Dir, true)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	defer s.Discard()
	said := strings.Join(s.Report.Cautions, "\n")
	if !strings.Contains(said, "can't be read") || !strings.Contains(said, "revoked") {
		t.Errorf("cautions: %q", said)
	}
}

// A gateway mid-write is the ordinary case: a journal exists, and is not a crash. The backup must
// succeed, name the journal, and hold only what has been committed.
func TestBackupDuringAWriteHoldsOnlyWhatIsFinished(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	dbPath := filepath.Join(r.Dir, "local-base.db")
	writer, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	writer.SetMaxOpenConns(1)
	tx, err := writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE api_keys SET name = 'not finished'`); err != nil {
		t.Fatal(err)
	}
	if !exists(dbPath + "-journal") {
		t.Fatal("the write has no journal, so this tests nothing")
	}
	res := create(t, r, false)
	found := false
	for _, n := range res.Manifest.NotIncluded {
		if n.What == "local-base.db-journal" {
			found = true
			if n.Unknown || n.Why == "" {
				t.Errorf("the journal is listed as %+v", n)
			}
		}
	}
	if !found {
		t.Error("the journal is not listed")
	}
	for _, e := range read(t, res.Output) {
		if e.header.Name != filesPrefix+"local-base.db" {
			continue
		}
		copied := filepath.Join(dir(t), "copied.db")
		if err := os.WriteFile(copied, e.body, 0o600); err != nil {
			t.Fatal(err)
		}
		db, err := openReadOnly(copied)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE name = 'not finished'`).Scan(&n); err != nil || n != 0 {
			t.Errorf("the backup holds an unfinished write: %d %v", n, err)
		}
	}
}
