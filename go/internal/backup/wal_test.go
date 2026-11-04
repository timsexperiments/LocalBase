package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

const (
	crashDatabaseEnv   = "LOCALBASE_TEST_CRASH_DATABASE"
	crashStatementsEnv = "LOCALBASE_TEST_CRASH_STATEMENTS"
	crashUnfinishedEnv = "LOCALBASE_TEST_CRASH_UNFINISHED"
	revokeAllKeys      = `UPDATE api_keys SET revoked_at = '2026-11-02T00:00:00.000Z' WHERE revoked_at IS NULL`
)

// TestHelperCrashWriter isn't a test of its own. crashWhileWriting runs it as a process, which dies
// here the way a gateway does when it's killed.
func TestHelperCrashWriter(t *testing.T) {
	path := os.Getenv(crashDatabaseEnv)
	if path == "" {
		t.Skip("only crashWhileWriting runs this")
	}
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.DB.SetMaxOpenConns(1)
	for _, statement := range []string{`PRAGMA wal_autocheckpoint=0`, `PRAGMA cache_size=8`} {
		if _, err := st.DB.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range strings.Split(os.Getenv(crashStatementsEnv), "\x1f") {
		if statement == "" {
			continue
		}
		if _, err := st.DB.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if os.Getenv(crashUnfinishedEnv) != "" {
		tx, err := st.DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		// Larger than the page cache, so these pages are written to the -wal before any commit.
		if _, err := tx.Exec(`CREATE TABLE crash_mid_write (x)`); err != nil {
			t.Fatal(err)
		}
		for range 64 {
			if _, err := tx.Exec(`INSERT INTO crash_mid_write VALUES (randomblob(65536))`); err != nil {
				t.Fatal(err)
			}
		}
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	select {}
}

// crashWhileWriting has a real process commit these statements, optionally start a large write and not
// finish it, and be killed. What it committed is then only in the -wal.
func crashWhileWriting(t *testing.T, dbPath string, unfinished bool, committed ...string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperCrashWriter$")
	cmd.Env = append(os.Environ(), crashDatabaseEnv+"="+dbPath, crashStatementsEnv+"="+strings.Join(committed, "\x1f"))
	if unfinished {
		cmd.Env = append(cmd.Env, crashUnfinishedEnv+"=1")
	}
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("the writer wasn't killed: %v\n%s", err, out)
	}
	info, err := os.Stat(dbPath + "-wal")
	if err != nil || info.Size() == 0 {
		t.Fatalf("no -wal after the crash, so this tests nothing: %v", err)
	}
}

func revokedKeys(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE revoked_at IS NOT NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func unfinishedTable(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'crash_mid_write'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func wholeOf(t *testing.T, db *sql.DB) {
	t.Helper()
	var result string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&result); err != nil || result != "ok" {
		t.Fatalf("integrity: %q %v", result, err)
	}
}

// databaseIn takes the database out of a backup and opens it.
func databaseIn(t *testing.T, archive string) *sql.DB {
	t.Helper()
	for _, e := range read(t, archive) {
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
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	t.Fatal("the backup holds no database")
	return nil
}

// The helper has to make what it says it makes, or every test using it proves nothing: a committed
// write that isn't in the main file, an unfinished one that was spilled, and a read that isn't refused.
func TestACrashLeavesItsWriteOnlyInTheWAL(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	crashWhileWriting(t, r.DB(), true, revokeAllKeys)

	body, err := os.ReadFile(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	alone := filepath.Join(dir(t), "alone.db")
	if err := os.WriteFile(alone, body, 0o600); err != nil {
		t.Fatal(err)
	}
	main, err := openReadOnly(alone)
	if err != nil {
		t.Fatal(err)
	}
	defer main.Close()
	if n := revokedKeys(t, main); n != 0 {
		t.Errorf("the write reached the main file, so it isn't only in the -wal: %d revoked", n)
	}

	if err := readableDatabase(context.Background(), r.DB()); err != nil {
		t.Errorf("a read-only open of a crashed root in write-ahead logging: %v", err)
	}
	withWAL, err := openReadOnly(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer withWAL.Close()
	if n := revokedKeys(t, withWAL); n == 0 {
		t.Error("a read doesn't see what was committed before the crash")
	}
	if n := unfinishedTable(t, withWAL); n != 0 {
		t.Error("a read sees what was never committed")
	}
	wholeOf(t, withWAL)
}

// SQLite recovers a -wal by itself, and what's read is a consistent state, so this isn't refused as a
// rollback journal is. The backup holds what was committed, and nothing that wasn't.
func TestBackupOfACrashedRootHoldsOnlyWhatWasCommitted(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	crashWhileWriting(t, r.DB(), true, revokeAllKeys)
	res := create(t, r, false)
	copied := databaseIn(t, res.Output)
	wholeOf(t, copied)
	if n := revokedKeys(t, copied); n == 0 {
		t.Error("the backup lost a write that was committed before the crash")
	}
	if n := unfinishedTable(t, copied); n != 0 {
		t.Error("the backup holds a write that was never committed")
	}
}

// A restore over a crashed root: what the root had goes aside whole, and nothing of it stays beside the
// restored database. Reading a revocation from the -wal also keeps it.
func TestRestoreOverACrashedRootKeepsItsWrite(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	older := create(t, r, false)
	crashWhileWriting(t, r.DB(), false, revokeAllKeys)
	s, err := Stage(context.Background(), older.Output, r.Dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if s.Report.RevocationsKept == 0 {
		t.Error("a revocation only in the -wal wasn't seen, so it would come back")
	}
	done, err := s.Commit()
	if err != nil {
		t.Fatal(err)
	}
	noWorkingFilesBeside(t, r.DB())
	restored, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	wholeOf(t, restored.DB)
	if n := revokedKeys(t, restored.DB); n == 0 {
		t.Error("the revocation didn't survive the restore")
	}
	aside, err := store.Open(filepath.Join(done.BeforeDir, "local-base.db"))
	if err != nil {
		t.Fatalf("the old database, opened where it was moved: %v", err)
	}
	defer aside.Close()
	wholeOf(t, aside.DB)
	if n := revokedKeys(t, aside.DB); n == 0 {
		t.Error("the moved copy lost a write that was committed before the crash")
	}
}

// noWorkingFilesBeside is the check that matters: a -wal left beside a database gets replayed into it,
// and `integrity_check` passes afterwards. The store's own check can't catch one from an older copy of
// the same database.
func noWorkingFilesBeside(t *testing.T, dbPath string) {
	t.Helper()
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if exists(dbPath + suffix) {
			t.Errorf("%s is beside the restored database", filepath.Base(dbPath+suffix))
		}
	}
}

// Whatever belongs with a database goes where it goes. Each is here with bytes of its own, so that
// a mix-up shows, and the old ones must come out unchanged on the other side.
func TestEverySidecarLeavesWithItsDatabase(t *testing.T) {
	// A root that was read for the report is held and switched out of write-ahead logging first, and SQLite
	// deals with its own files then. One that couldn't be read is moved as it is, with all of them.
	for _, c := range []struct {
		name      string
		couldntBe bool
		ownFiles  []string
	}{
		{name: "a root that could be read", ownFiles: []string{"-wal", "-shm", "-journal"}},
		{name: "a root that couldn't", couldntBe: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, _ := newRoot(t, dir(t))
			res := create(t, r, false)
			s, err := Stage(context.Background(), res.Output, r.Dir, true)
			if err != nil {
				t.Fatal(err)
			}
			if c.couldntBe {
				s.live = nil
			}
			planted := map[string]string{}
			for _, suffix := range store.Sidecars {
				// An empty -wal and -journal, so that nothing replays them, and this checks only where they go.
				body := ""
				if suffix != "-wal" && suffix != "-journal" && suffix != "-shm" {
					body = "planted " + suffix
				}
				planted[suffix] = body
				if err := os.WriteFile(r.DB()+suffix, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			done, err := s.Commit()
			if err != nil {
				t.Fatal(err)
			}
			noWorkingFilesBeside(t, r.DB())
			for suffix, body := range planted {
				if contains(c.ownFiles, suffix) {
					continue
				}
				got, err := os.ReadFile(filepath.Join(done.BeforeDir, "local-base.db"+suffix))
				if err != nil || string(got) != body {
					t.Errorf("%s in the old files: %q %v", suffix, got, err)
				}
			}
			if got, err := os.ReadFile(r.DB() + "-wal-owner"); err == nil && strings.HasPrefix(string(got), "planted") {
				t.Errorf("the old owner file is beside the restored database: %q", got)
			}
		})
	}
}

// An undone restore puts each of them back as it was, byte for byte, and removes what the restored
// database wrote. The database itself comes back with its header changed, which isPutBack says exactly.
func TestAnUndoneRestorePutsEverySidecarBack(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	res := create(t, r, false)
	s, err := Stage(context.Background(), res.Output, r.Dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.DB()+"-wal-owner", []byte("planted owner"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := rootAsItIs(t, r.Dir, "runtime")
	was := stepHook
	// After the checks, which open the restored database and so write its owner file.
	stepHook = func(step string) error {
		if step == "finish" {
			return errors.New("injected")
		}
		return nil
	}
	defer func() { stepHook = was }()
	if _, err := s.Commit(); err == nil || !strings.Contains(err.Error(), "put back") {
		t.Fatalf("commit: %v", err)
	}
	before.isPutBack(t, r.Dir, "runtime")
}

// A file can't bring its own: a -wal in a backup would be replayed into the database it names.
func TestABackupCarryingASidecarIsRefused(t *testing.T) {
	for _, suffix := range store.Sidecars {
		t.Run(suffix, func(t *testing.T) {
			r, _ := newRoot(t, dir(t))
			e := read(t, create(t, r, false).Output)
			name := filesPrefix + "local-base.db" + suffix
			e = append(e, entry{header: &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600}, body: []byte("carried")})
			reseal(t, e)
			bad := filepath.Join(dir(t), "b"+Extension)
			pack(t, bad, e)
			s, err := Stage(context.Background(), bad, filepath.Join(dir(t), "target"), false)
			if err == nil {
				s.Discard()
				t.Fatal("it was accepted")
			}
			if !strings.Contains(err.Error(), "local-base.db"+suffix) {
				t.Errorf("it doesn't name the file: %v", err)
			}
		})
	}
}

// One list says what belongs with a database. A file added to it has to be handled here too.
func TestEverySidecarIsHandled(t *testing.T) {
	for _, suffix := range store.Sidecars {
		name := "local-base.db" + suffix
		if notIncludedTop[name] == "" {
			t.Errorf("a backup gives no reason for leaving out %s", name)
		}
		if !contains(replaceable, name) {
			t.Errorf("a restore doesn't move %s", name)
		}
	}
	if replaceable[0] != "local-base.db" {
		t.Errorf("the database goes in last, and that depends on it being first here: %v", replaceable)
	}
}

// stoppedCleanly leaves a root as a clean stop does, with only the main database file. The last
// connection to close takes the working files with it.
func stoppedCleanly(t *testing.T, dbPath string) {
	t.Helper()
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if exists(dbPath + suffix) {
			t.Fatalf("%s is still there after a clean stop, so this tests nothing", suffix)
		}
	}
}

// A dry run promises no row changed and nothing left behind. A read-only open of a database in
// write-ahead logging makes a -shm and a -wal, and they stay, so this can't read the root that way.
func TestADryRunLeavesTheRootAsItWas(t *testing.T) {
	ctx := context.Background()
	t.Run("a stopped root", func(t *testing.T) {
		r, _ := newRoot(t, dir(t))
		older := create(t, r, false)
		stoppedCleanly(t, r.DB())
		before := tree(t, r.Dir, "runtime", "logs")
		if _, err := Create(ctx, Options{Root: r, Output: filepath.Join(dir(t), "x"+Extension), DryRun: true}); err != nil {
			t.Fatal(err)
		}
		if after := tree(t, r.Dir, "runtime", "logs"); after != before {
			t.Errorf("a dry-run backup changed the root:\n%s\nwas:\n%s", after, before)
		}
		s, err := Stage(ctx, older.Output, r.Dir, true)
		if err != nil {
			t.Fatal(err)
		}
		s.Discard()
		if after := tree(t, r.Dir, "runtime", "logs"); after != before {
			t.Errorf("staging a restore changed the root:\n%s\nwas:\n%s", after, before)
		}
	})
	t.Run("a root a process was killed on", func(t *testing.T) {
		r, _ := newRoot(t, dir(t))
		older := create(t, r, false)
		crashWhileWriting(t, r.DB(), false, revokeAllKeys)
		wal, err := os.ReadFile(r.DB() + "-wal")
		if err != nil {
			t.Fatal(err)
		}
		main, err := os.ReadFile(r.DB())
		if err != nil {
			t.Fatal(err)
		}
		dry, err := Create(ctx, Options{Root: r, Output: filepath.Join(dir(t), "x"+Extension), DryRun: true})
		if err != nil || dry == nil {
			t.Fatal(err)
		}
		s, err := Stage(ctx, older.Output, r.Dir, true)
		if err != nil {
			t.Fatal(err)
		}
		// It still sees the recent revocation, which `immutable` wouldn't.
		if s.Report.RevocationsKept == 0 {
			t.Error("a revocation only in the -wal wasn't seen")
		}
		s.Discard()
		if after, _ := os.ReadFile(r.DB() + "-wal"); string(after) != string(wal) {
			t.Error("reading changed the -wal")
		}
		if after, _ := os.ReadFile(r.DB()); string(after) != string(main) {
			t.Error("reading changed the database")
		}
	})
}

// A root that can't be written can still be backed up and looked at. Stopped, 0500 and 0400.
func TestADryRunWorksOnARootThatCantBeWritten(t *testing.T) {
	ctx := context.Background()
	r, _ := newRoot(t, dir(t))
	older := create(t, r, false)
	stoppedCleanly(t, r.DB())
	for _, name := range []string{"local-base.db", "local-base.db-wal-owner"} {
		if err := os.Chmod(filepath.Join(r.Dir, name), 0o400); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(r.Dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(r.Dir, 0o700) })
	if _, err := Create(ctx, Options{Root: r, Output: filepath.Join(dir(t), "x"+Extension), DryRun: true, ExcludeSecrets: true}); err != nil {
		t.Errorf("backup --dry-run --exclude-secrets: %v", err)
	}
	s, err := Stage(ctx, older.Output, r.Dir, true)
	if err != nil {
		t.Errorf("restore --dry-run: %v", err)
	} else {
		s.Discard()
	}
}

// A backup that isn't a dry run reads the root too, and leaves it as it found it.
func TestABackupLeavesNoWorkingFilesBehind(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	stoppedCleanly(t, r.DB())
	create(t, r, false)
	noWorkingFilesBeside(t, r.DB())
}

// Restoring over a root has to compare with it whatever state its -shm is in. If that read failed, the
// comparison would be skipped as "can't be read", and a revoked key would come back with only a caution.
func TestAnUnusableShmDoesntSkipTheComparison(t *testing.T) {
	for name, spoil := range map[string]func(t *testing.T, path string){
		"nonsense": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(strings.Repeat("not a shared memory file ", 2000)), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"empty": func(t *testing.T, path string) {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"unreadable": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("x"), 0o000); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, _ := newRoot(t, dir(t))
			older := create(t, r, false)
			crashWhileWriting(t, r.DB(), false, revokeAllKeys)
			if err := os.Remove(r.DB() + "-shm"); err != nil {
				t.Fatal(err)
			}
			spoil(t, r.DB()+"-shm")
			t.Cleanup(func() { _ = os.Chmod(r.DB()+"-shm", 0o600) })
			s, err := Stage(context.Background(), older.Output, r.Dir, true)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Discard()
			if s.Report.RevocationsKept == 0 {
				t.Errorf("the comparison was skipped: cautions %q", s.Report.Cautions)
			}
		})
	}
}

// Closing the connection a restore holds deletes whatever journal is at its path, even with its own database
// moved away. So it has to close while the restore's journal still keeps every command out, or it could take
// the journal of a command that had just started. That goes for both ways out.
func TestTheHeldDatabaseClosesWhileCommandsAreStillKeptOut(t *testing.T) {
	for name, fails := range map[string]bool{"finishing": false, "going back": true} {
		t.Run(name, func(t *testing.T) {
			r, _ := newRoot(t, dir(t))
			s := stageOver(t, r)
			journal := r.DB() + "-journal"
			was := stepHook
			defer func() { stepHook = was }()
			closed, keptOut := false, false
			stepHook = func(at string) error {
				switch at {
				case "finish":
					if err := os.WriteFile(journal, []byte("a command's"), 0o600); err != nil {
						t.Fatal(err)
					}
					if fails {
						return errors.New("injected")
					}
				case "released", "undo released":
					closed, keptOut = !exists(journal), exists(journalPath(r.Dir))
				}
				return nil
			}
			if _, err := s.Commit(); (err != nil) != fails {
				t.Fatalf("commit: %v", err)
			}
			if !closed {
				t.Error("the connection wasn't closed by then")
			}
			if !keptOut {
				t.Error("the restore's journal had already gone")
			}
		})
	}
}

// rootWas is a root as it was, to compare with one that a restore went back on. The database is checked
// apart, because a restore takes it out of write-ahead logging to move it, which rewrites part of its
// header. Its next open puts it back.
type rootWas struct {
	rest     string
	database []byte
}

var databaseFiles = []string{"local-base.db", "local-base.db-wal", "local-base.db-shm", "local-base.db-journal"}

func rootAsItIs(t *testing.T, rootDir string, skip ...string) rootWas {
	t.Helper()
	database, err := os.ReadFile(filepath.Join(rootDir, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	return rootWas{tree(t, rootDir, append(skip, databaseFiles...)...), database}
}

// headerBytesTheSwitchChanges are the file format versions, the change counter, and the version that wrote it.
func headerBytesTheSwitchChanges(at int) bool {
	return at == 18 || at == 19 || (at >= 24 && at <= 27) || (at >= 92 && at <= 99)
}

// isPutBack checks everything but the database byte for byte, and the database down to those header bytes.
// All a killed process may leave beside it is what holds nothing: an empty -wal, or a journal SQLite has
// finished with, which it ignores.
func (was rootWas) isPutBack(t *testing.T, rootDir string, skip ...string) {
	t.Helper()
	if rest := tree(t, rootDir, append(skip, databaseFiles...)...); rest != was.rest {
		t.Errorf("not put back:\n%s\nwas:\n%s", rest, was.rest)
	}
	dbPath := filepath.Join(rootDir, "local-base.db")
	now, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(now) != len(was.database) {
		t.Errorf("the database is %d bytes, and was %d", len(now), len(was.database))
	} else {
		for at := range now {
			if now[at] != was.database[at] && !headerBytesTheSwitchChanges(at) {
				t.Errorf("the database differs at byte %d", at)
				break
			}
		}
	}
	if info, err := os.Stat(dbPath + "-wal"); err == nil && info.Size() != 0 {
		t.Errorf("a -wal with %d bytes is beside it", info.Size())
	}
	if exists(dbPath + "-shm") {
		t.Error("a -shm is beside it")
	}
	if journal, err := os.ReadFile(dbPath + "-journal"); err == nil && !bytes.Equal(journal[:min(len(journal), 8)], make([]byte, min(len(journal), 8))) {
		t.Error("a journal SQLite would play is beside it")
	}
}

// takesAWrite opens a database as a command does, writes to it, and checks it's in write-ahead logging.
func takesAWrite(t *testing.T, dbPath string) {
	t.Helper()
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`UPDATE api_keys SET name = name`); err != nil {
		t.Errorf("a write: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if !store.InWAL(dbPath) {
		t.Error("it didn't go back into write-ahead logging")
	}
}

// A -shm left over can't mislead a database opened afterwards, whether it's rubbish or another database's.
// SQLite rebuilds it for the first connection. Reopening in write-ahead logging and writing is what shows it,
// since a read-only look or a database in rollback mode wouldn't use it at all.
func TestAStaleShmDoesNoHarm(t *testing.T) {
	other, _ := newRoot(t, dir(t))
	open, err := store.Open(other.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	if _, err := open.DB.Exec(`UPDATE api_keys SET name = 'in the other database'`); err != nil {
		t.Fatal(err)
	}
	anothers, err := os.ReadFile(other.DB() + "-shm")
	if err != nil || len(anothers) == 0 {
		t.Fatalf("the other database has no -shm to borrow: %v", err)
	}
	for name, body := range map[string][]byte{"rubbish": bytes.Repeat([]byte("rubbish!"), 4096), "another database's": anothers} {
		t.Run(name, func(t *testing.T) {
			r, _ := newRoot(t, dir(t))
			stoppedCleanly(t, r.DB())
			if err := os.WriteFile(r.DB()+"-shm", body, 0o600); err != nil {
				t.Fatal(err)
			}
			keys := keyCount(t, r.DB())
			takesAWrite(t, r.DB())
			st, err := store.Open(r.DB())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB.Exec(`UPDATE api_keys SET name = 'written here'`); err != nil {
				t.Errorf("a write: %v", err)
			}
			var written int
			if err := st.DB.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE name = 'written here'`).Scan(&written); err != nil {
				t.Fatal(err)
			}
			_ = st.Close()
			if written != keys {
				t.Errorf("%d of %d rows have the write", written, keys)
			}
			wholeDatabase(t, r.DB())
		})
	}
}

// A backup's database is in rollback mode, however its root was. It can't need a -wal, and what it holds
// comes up in write-ahead logging when a command opens it.
func TestABackupsDatabaseIsNotInWAL(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	res := create(t, r, false)
	if !store.InWAL(r.DB()) {
		t.Fatal("the root isn't in write-ahead logging, so this tests nothing")
	}
	for _, e := range read(t, res.Output) {
		if e.header.Name != filesPrefix+"local-base.db" {
			continue
		}
		if len(e.body) < 20 || e.body[18] != 1 || e.body[19] != 1 {
			t.Errorf("its header says format %d and %d", e.body[18], e.body[19])
		}
		return
	}
	t.Fatal("the backup holds no database")
}

// The owner file beside a restored database is that database's, or isn't there. Another's would make SQLite's
// -wal look as if it belonged, which is what the file exists to catch.
func TestARestoredRootKeepsNoOwnerFromBefore(t *testing.T) {
	from, _ := newRoot(t, dir(t))
	res := create(t, from, false)
	r, _ := newRoot(t, dir(t))
	stoppedCleanly(t, r.DB())
	old, err := os.ReadFile(r.DB() + "-wal-owner")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Stage(context.Background(), res.Output, r.Dir, true)
	if err != nil {
		t.Fatal(err)
	}
	done, err := s.Commit()
	if err != nil {
		t.Fatal(err)
	}
	moved, err := os.ReadFile(filepath.Join(done.BeforeDir, "local-base.db-wal-owner"))
	if err != nil || !bytes.Equal(moved, old) {
		t.Errorf("the old owner file in the old files: %q %v", moved, err)
	}
	now, err := os.ReadFile(r.DB() + "-wal-owner")
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if fields, oldFields := strings.Fields(string(now)), strings.Fields(string(old)); len(fields) == 0 || len(oldFields) == 0 || fields[0] == oldFields[0] {
		t.Errorf("the owner file beside the restored database is %q, and the old one was %q", now, old)
	}
	takesAWrite(t, r.DB())
}
