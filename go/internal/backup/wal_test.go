package backup

import (
	"archive/tar"
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
	r, _ := newRoot(t, dir(t))
	res := create(t, r, false)
	s, err := Stage(context.Background(), res.Output, r.Dir, true)
	if err != nil {
		t.Fatal(err)
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
		got, err := os.ReadFile(filepath.Join(done.BeforeDir, "local-base.db"+suffix))
		if err != nil || string(got) != body {
			t.Errorf("%s in the old files: %q %v", suffix, got, err)
		}
	}
	if got, err := os.ReadFile(r.DB() + "-wal-owner"); err == nil && strings.HasPrefix(string(got), "planted") {
		t.Errorf("the old owner file is beside the restored database: %q", got)
	}
}

// An undone restore puts each of them back as it was, byte for byte, and removes what the restored
// database wrote.
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
	before := tree(t, r.Dir, "runtime")
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
	if after := tree(t, r.Dir, "runtime"); after != before {
		t.Errorf("after an undo:\n%s\nwas:\n%s", after, before)
	}
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
