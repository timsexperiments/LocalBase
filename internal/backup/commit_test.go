package backup

import (
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
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

func stageOver(t *testing.T, r *root.Root) *Staged {
	t.Helper()
	res := create(t, r, false)
	s, err := Stage(context.Background(), res.Output, r.Dir, true)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func changeRoot(t *testing.T, r *root.Root, statement string) {
	t.Helper()
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.DB.Exec(statement); err != nil {
		t.Fatal(err)
	}
}

// Everything the report told the person is compared again after they answer, and each kind of change
// stops the restore by name, with the root's files exactly as they were.
func TestCommitStopsWhenWhatTheReportSaidHasChanged(t *testing.T) {
	for _, c := range []struct {
		name, statement, says string
	}{
		{"a key revoked", `UPDATE api_keys SET revoked_at = 'now'`, "API keys"},
		{"a key rotated", `UPDATE api_keys SET key_hash = 'another', last_rotated_at = 'now'`, "API keys"},
		{"a key narrowed", `UPDATE api_keys SET scopes = '[]'`, "API keys"},
		{"a key renamed", `UPDATE api_keys SET name = 'another'`, "API keys"},
		{"a key removed", `DELETE FROM api_keys`, "API keys"},
		{"a key added", `INSERT INTO api_keys (id, name, prefix, key_hash, scopes, created_at, last_rotated_at, expires_at, revoked_at) SELECT 'key_new', name, 'p2', 'h2', scopes, created_at, last_rotated_at, expires_at, revoked_at FROM api_keys LIMIT 1`, "API keys"},
		{"a user disabled", `UPDATE auth_users SET status = 'disabled'`, "users"},
		{"a user added", `INSERT INTO auth_users (id, status, created_at, updated_at) VALUES ('user-2', 'active', 'x', 'x')`, "users"},
		{"a role added", `INSERT INTO auth_roles (id, name, description, created_at, updated_at) VALUES ('r', 'r', 'r', 'x', 'x')`, "roles"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, _ := newRoot(t, dir(t))
			s := stageOver(t, r)
			changeRoot(t, r, c.statement)
			before := tree(t, r.Dir, "runtime")
			_, err := s.Commit()
			var refused *Refused
			// Exactly the refusal, and not the same words wrapped by a failure that went through the undo.
			if !errors.As(err, &refused) || !strings.HasPrefix(err.Error(), "This root changed while this waited: ") || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("commit: %v", err)
			}
			if after := tree(t, r.Dir, "runtime"); after != before {
				t.Errorf("it stopped, and the root changed:\n%s\nwas:\n%s", after, before)
			}
			if exists(journalPath(r.Dir)) {
				t.Error("it stopped, and left its journal")
			}
			if _, err := root.Open(r.Dir); err != nil {
				t.Errorf("the root after it stopped: %v", err)
			}
			if err := Undo(r.Dir); err == nil || !strings.Contains(err.Error(), "no unfinished restore") {
				t.Errorf("--undo after it stopped: %v", err)
			}
		})
	}
}

// So does what decides who can sign in, which is in files.
func TestCommitStopsWhenTheSignInSettingsChanged(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	s := stageOver(t, r)
	path := filepath.Join(r.Dir, "ui-access.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(body), "https://accounts.example.com", "https://other.example.com")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(); err == nil || !strings.Contains(err.Error(), "changed while this waited") || !strings.Contains(err.Error(), "sign-in") {
		t.Fatalf("commit: %v", err)
	}
}

// A stop can't touch what isn't its own. A process may be writing here, and these are its working files:
// removing them would lose what it has committed.
func TestStoppingLeavesAnotherProcesssWorkingFilesAlone(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	s := stageOver(t, r)
	s.opts = shortBusyWaits(0)
	release := holdLock(t, r.DB())
	defer release()
	// Empty, so that SQLite ignores it and this checks only whether a stop removes it.
	if err := os.WriteFile(r.DB()+"-journal", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(); err == nil {
		t.Fatal("it went ahead with the database held")
	}
	if !exists(r.DB() + "-journal") {
		t.Error("a stop removed a working file that was never its own")
	}
}

// The same for an undo before anything new went in: a kill between writing the journal and the move
// leaves whatever a process has made since, and an undo can't take any of it for this restore's.
func TestAnUndoBeforeAnythingWentInLeavesTheRestAlone(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, r *root.Root, j *journal)
	}{
		{"nothing moved", func(t *testing.T, r *root.Root, j *journal) {}},
		{"the database moved, and not the settings", func(t *testing.T, r *root.Root, j *journal) {
			// A later run of an undo, or a kill partway through the move aside.
			before := filepath.Join(r.Dir, j.Before)
			if err := os.Mkdir(before, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(r.DB(), filepath.Join(before, "local-base.db")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(r.DB(), []byte("not the database"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, _ := newRoot(t, dir(t))
			s := stageOver(t, r)
			defer s.Discard()
			// The journal lists the database and one more, which hasn't moved.
			j := &journal{Version: 1, Before: beforePrefix + "20260101-000000", Had: []string{"local-base.db", "voices"}}
			c.setup(t, r, j)
			made := map[string]string{
				"local-base.db-wal": "someone's writes",
				"ui-access.json":    "made while it waited",
			}
			for name, body := range made {
				if err := os.WriteFile(filepath.Join(r.Dir, name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := undo(r.Dir, j, func() {}, options{}); err != nil {
				t.Fatal(err)
			}
			for name, body := range made {
				if got, err := os.ReadFile(filepath.Join(r.Dir, name)); err != nil || string(got) != body {
					t.Errorf("an undo touched %s: %q %v", name, got, err)
				}
			}
			if exists(filepath.Join(r.Dir, j.Before)) {
				t.Error("the directory of old files is still there")
			}
			wholeDatabase(t, r.DB())
		})
	}
}

// An undo that's stopped partway leaves a root the next one can finish, whichever step it was, and that
// root is the one the restore started from, byte for byte.
func TestAnUndoStoppedPartwayCanBeRunAgain(t *testing.T) {
	var steps []string
	func() {
		r, _ := newRoot(t, dir(t))
		s := stageOver(t, r)
		s.opts = options{step: func(step string) error {
			if strings.HasPrefix(step, "undo ") {
				steps = append(steps, step)
			}
			if step == "finish" {
				return errors.New("injected")
			}
			return nil
		}}
		if _, err := s.Commit(); err == nil {
			t.Fatal("no failure")
		}
	}()
	// Then only the release, which has to stay after everything is back.
	if len(steps) < 2 || steps[len(steps)-2] != "undo back local-base.db" || steps[len(steps)-1] != "undo released" {
		t.Fatalf("the database should go back last: %v", steps)
	}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			r, _ := newRoot(t, dir(t))
			s := stageOver(t, r)
			before := rootAsItIs(t, r.Dir, "runtime")
			// Commit fails after it has moved everything, and its undo crashes at this step.
			s.opts = options{step: func(at string) error {
				switch at {
				case "finish":
					return errors.New("injected")
				case step:
					panic("crash")
				}
				return nil
			}}
			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("no crash")
					}
				}()
				_, _ = s.Commit()
			}()
			if _, err := root.Open(r.Dir); err == nil {
				t.Error("the root opened with a restore half undone")
			}
			if err := Undo(r.Dir); err != nil {
				t.Fatalf("the second undo: %v", err)
			}
			before.isPutBack(t, r.Dir, "runtime")
			if _, err := root.Open(r.Dir); err != nil {
				t.Errorf("the root afterwards: %v", err)
			}
			takesAWrite(t, r.DB())
		})
	}
}

// A new root has no database in its journal. Killed once the restored files are in, an undo has to
// remove all of them, including what the restored database made beside itself.
func TestAnUndoOnANewRootLeavesNothing(t *testing.T) {
	src, _ := newRoot(t, dir(t))
	res := create(t, src, false)
	parent := dir(t)
	target := filepath.Join(parent, "target")
	s, err := Stage(context.Background(), res.Output, target, false)
	if err != nil {
		t.Fatal(err)
	}
	s.opts = options{step: func(step string) error {
		if step == "finish" {
			for _, suffix := range []string{"-wal", "-shm", "-journal"} {
				if err := os.WriteFile(filepath.Join(target, "local-base.db"+suffix), []byte("the restored database's"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			panic("crash")
		}
		return nil
	}}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("no crash")
			}
		}()
		_, _ = s.Commit()
	}()
	if err := Undo(target); err != nil {
		t.Fatal(err)
	}
	if exists(target) {
		entries, _ := os.ReadDir(target)
		t.Errorf("an undo on a new root left: %v", entries)
	}
}

// Something the report couldn't read doesn't turn the rest of the comparison off: the keys were read,
// so a key revoked while it asked is still seen.
func TestAPartlyUnreadableRootIsStillCompared(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	res := create(t, r, false)
	changeRoot(t, r, `ALTER TABLE auth_role_permissions RENAME COLUMN permission TO granted`)
	s, err := Stage(context.Background(), res.Output, r.Dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if s.live == nil {
		t.Fatal("a root whose keys could be read has nothing to compare")
	}
	changeRoot(t, r, `UPDATE api_keys SET revoked_at = 'now'`)
	if _, err := s.Commit(); err == nil || !strings.Contains(err.Error(), "changed while this waited") || !strings.Contains(err.Error(), "API keys") {
		t.Fatalf("commit: %v", err)
	}
}

// The control. What couldn't be read is compared as a fact, between a read-only read for the report and
// a read-write one here. If those ever described it differently, a broken root would report a change on
// every try, and could never be restored, which is often why someone is restoring it.
func TestAPartlyUnreadableRootThatDidntChangeIsRestored(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	res := create(t, r, false)
	changeRoot(t, r, `ALTER TABLE auth_role_permissions RENAME COLUMN permission TO granted`)
	s, err := Stage(context.Background(), res.Output, r.Dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if s.live == nil {
		t.Fatal("a root whose keys could be read has nothing to compare")
	}
	if s.live.signIn.unreadable == strings.Repeat(";", 3) {
		t.Fatal("nothing failed to read, so this tests nothing")
	}
	if _, err := s.Commit(); err != nil {
		t.Fatalf("a root that didn't change: %v", err)
	}
	wholeDatabase(t, r.DB())
	if exists(journalPath(r.Dir)) {
		t.Error("it finished, and left its journal")
	}
}

// The journal is written twice, and an undo depends on what it says, so the second one has to reach the
// disk before anything moves, as the first does. Lost in a power cut, it could list a working file that's
// gone since, and an undo would then never see everything aside, and would leave new files behind.
func TestTheSecondJournalReachesTheDiskBeforeAnythingMoves(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	s := stageOver(t, r)
	calls, movedBy := 0, map[int]bool{}
	s.opts = options{dirSync: func(d *os.File) error {
		calls++
		aside, err := filepath.Glob(filepath.Join(r.Dir, beforePrefix+"*"))
		if err != nil {
			t.Fatal(err)
		}
		movedBy[calls] = len(aside) > 0
		return d.Sync()
	}}
	if _, err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	if calls < 2 || movedBy[1] || movedBy[2] {
		t.Errorf("the journal's two writes should each be synced before anything moves. %d syncs, moved by each: %v", calls, movedBy)
	}
}

// A database deleted while it asked isn't created empty, and it says so rather than that nothing changed
// with an empty file left behind.
func TestCommitDoesntCreateADatabaseDeletedWhileItAsked(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	s := stageOver(t, r)
	if err := os.Remove(r.DB()); err != nil {
		t.Fatal(err)
	}
	_, err := s.Commit()
	var refused *Refused
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "can't be read now") {
		t.Fatalf("commit: %v", err)
	}
	if exists(r.DB()) {
		t.Error("it created the database")
	}
}

// A root that had nothing to replace when the report was written, and has now.
func TestCommitStopsWhenAnEmptyRootGainedSettings(t *testing.T) {
	src, _ := newRoot(t, dir(t))
	res := create(t, src, false)
	target, err := root.Init(filepath.Join(dir(t), "target"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Stage(context.Background(), res.Output, target.Dir, true)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(target.DB())
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if _, err := s.Commit(); err == nil || !strings.Contains(err.Error(), "changed while this waited") {
		t.Fatalf("commit: %v", err)
	}
}

// With nothing changed it goes ahead, and leaves nothing held.
func TestCommitGoesAheadWhenNothingChanged(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	s := stageOver(t, r)
	if _, err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	if exists(journalPath(r.Dir)) {
		t.Error("it finished, and left its journal")
	}
	wholeDatabase(t, r.DB())
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.DB.Exec(`UPDATE api_keys SET name = name`); err != nil {
		t.Errorf("a write after it finished: %v", err)
	}
}

// Something holding the database too long stops it, plainly, with nothing changed. The holder here is a
// connection in this process taking the lock itself, so this is the same process only, and doesn't show what a
// process that opened through the store does. That's TestAnIdleProcessWithTheDatabaseOpenStopsARestore.
func TestCommitStopsWhenSomethingElseHoldsTheDatabase(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	s := stageOver(t, r)
	s.opts = shortBusyWaits(0)
	release := holdLock(t, r.DB())
	defer release()
	before := tree(t, r.Dir, "runtime")
	_, err := s.Commit()
	var refused *Refused
	if !errors.As(err, &refused) || err.Error() != someoneElseHoldsIt {
		t.Fatalf("commit: %v", err)
	}
	if strings.Contains(err.Error(), "SQLITE_BUSY") || strings.Contains(err.Error(), "database is locked") {
		t.Errorf("the raw error reaches the person: %v", err)
	}
	if after := tree(t, r.Dir, "runtime"); after != before {
		t.Errorf("it stopped, and the root changed:\n%s\nwas:\n%s", after, before)
	}
}

// A process that had already opened the database, and writes during the move. It waits on the lock, and
// then fails, loudly. Neither database has the write, and nothing was lost without anyone being told.
func TestAWriteDuringTheMoveFailsAndIsNotLost(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	// As a root from before write-ahead logging is: its connection takes no lock until it's used, so one
	// can be open here. In write-ahead logging, TestAnIdleProcessWithTheDatabaseOpenStopsARestore applies.
	inRollbackMode(t, r.DB())
	s := stageOver(t, r)
	late, err := sql.Open("sqlite", "file:"+r.DB()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer late.Close()
	late.SetMaxOpenConns(1)
	var n int
	if err := late.QueryRow(`SELECT COUNT(*) FROM api_keys`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	wrote := make(chan error, 1)
	started := false
	s.opts = options{step: func(step string) error {
		if step == "before" && !started {
			started = true
			go func() {
				_, err := late.Exec(`UPDATE api_keys SET name = 'written during the move'`)
				wrote <- err
			}()
		}
		return nil
	}}
	done, err := s.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if err := <-wrote; err == nil {
		t.Error("the write succeeded, and went into a database that was moved away")
	}
	for name, path := range map[string]string{"restored": r.DB(), "moved": filepath.Join(done.BeforeDir, "local-base.db")} {
		st, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var count int
		if err := st.DB.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE name = 'written during the move'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		_ = st.Close()
		if count != 0 {
			t.Errorf("the %s database has the write", name)
		}
	}
}

// A key command started once the journal is there is refused, through the real command, and what it says
// tells someone a restore is running, and not to undo it.
func TestACommandStartedDuringARestoreIsRefused(t *testing.T) {
	bin := filepath.Join(dir(t), "local-base")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/localbase").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	r, _ := newRoot(t, dir(t))
	s := stageOver(t, r)
	var said string
	var ran error
	s.opts = options{step: func(step string) error {
		if step == "before" {
			out, err := exec.Command(bin, "--root", r.Dir, "keys", "list").CombinedOutput()
			said, ran = string(out), err
		}
		return nil
	}}
	if _, err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	if ran == nil {
		t.Errorf("keys list ran during a restore: %s", said)
	}
	if !strings.Contains(said, "A restore is running on") || !strings.Contains(said, "Wait for it to finish") {
		t.Errorf("it says: %s", said)
	}
}

// On the default root that refusal says so, as other messages about a running gateway do.
func TestACommandRefusedDuringARestoreOnTheDefaultRootSaysSo(t *testing.T) {
	bin := filepath.Join(dir(t), "local-base")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/localbase").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	src, _ := newRoot(t, dir(t))
	res := create(t, src, false)
	home, err := filepath.EvalSymlinks(dir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("LOCALBASE_ROOT", "")
	target := filepath.Join(home, ".local", "share", "local-base")
	s, err := Stage(context.Background(), res.Output, target, false)
	if err != nil {
		t.Fatal(err)
	}
	var said string
	s.opts = options{step: func(step string) error {
		if step == "before" {
			out, _ := exec.Command(bin, "--root", target, "keys", "list").CombinedOutput()
			said = string(out)
		}
		return nil
	}}
	if _, err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	want := "A restore is running on '" + target + "'. This is the default root, the one your installed LocalBase uses. Wait for it to finish, then run this again."
	if !strings.Contains(said, want) {
		t.Errorf("it says: %s", said)
	}
}

// TestHelperKilledCommit isn't a test of its own. A real process stages a restore, and is killed at a
// step of Commit, as a person's power cut or kill -9 would.
func TestHelperKilledCommit(t *testing.T) {
	step := os.Getenv("LOCALBASE_TEST_KILL_AT")
	if step == "" {
		t.Skip("only TestAKilledRestoreIsPutBack runs this")
	}
	hook := func(at string) error {
		if at == step {
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			select {}
		}
		return nil
	}
	s, err := Stage(context.Background(), os.Getenv("LOCALBASE_TEST_FILE"), os.Getenv("LOCALBASE_TEST_ROOT"), true)
	if err != nil {
		t.Fatal(err)
	}
	s.opts = options{step: hook}
	if _, err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	t.Fatal("it wasn't killed")
}

// Killed between writing the journal and comparing: every command refuses until --undo, as for any
// other point of a restore, and --undo leaves the root as it was.
func TestAKilledRestoreIsPutBack(t *testing.T) {
	for _, step := range []string{"hold", "compared", "before"} {
		t.Run(step, func(t *testing.T) {
			r, _ := newRoot(t, dir(t))
			res := create(t, r, false)
			before := rootAsItIs(t, r.Dir, "runtime")
			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperKilledCommit$")
			cmd.Env = append(os.Environ(), "LOCALBASE_TEST_KILL_AT="+step, "LOCALBASE_TEST_FILE="+res.Output, "LOCALBASE_TEST_ROOT="+r.Dir)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("it wasn't killed: %v\n%s", err, out)
			}
			if !exists(journalPath(r.Dir)) {
				t.Fatal("no journal, so this isn't that window")
			}
			if _, err := root.Open(r.Dir); err == nil || !strings.Contains(err.Error(), "restore --undo") {
				t.Errorf("the root after a kill: %v", err)
			}
			if err := Undo(r.Dir); err != nil {
				t.Fatalf("undo: %v", err)
			}
			before.isPutBack(t, r.Dir, "runtime")
			// Only the switch may change the header, and it comes after these two.
			if step != "before" {
				if now, err := os.ReadFile(r.DB()); err != nil || !bytes.Equal(now, before.database) {
					t.Errorf("killed before the switch, and the database changed: %v", err)
				}
			}
			wholeDatabase(t, r.DB())
			takesAWrite(t, r.DB())
		})
	}
}

// A connection in this process stops a restore, and what it writes is kept. This is the same process only:
// SQLite shares its locks within one, so this can't show what another process does. That's
// TestAnIdleProcessWithTheDatabaseOpenStopsARestore.
func TestAConnectionInThisProcessStopsARestore(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	s := stageOver(t, r)
	s.opts = shortBusyWaits(100 * time.Millisecond)
	open, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	var n int
	if err := open.DB.QueryRow(`SELECT COUNT(*) FROM api_keys`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	_, err = s.Commit()
	var refused *Refused
	if !errors.As(err, &refused) || err.Error() != someoneElseHoldsIt {
		t.Fatalf("commit: %v", err)
	}
	if exists(journalPath(r.Dir)) {
		t.Error("it stopped, and left its journal")
	}
	if _, err := open.DB.Exec(`UPDATE api_keys SET name = 'written after it stopped'`); err != nil {
		t.Errorf("the other process can't write: %v", err)
	}
	var written int
	if err := open.DB.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE name = 'written after it stopped'`).Scan(&written); err != nil {
		t.Fatal(err)
	}
	if written == 0 {
		t.Error("its write isn't there")
	}
}

// Once a restore holds a database in write-ahead logging, a process arriving late can't write to it, so
// its write is neither lost in the moved database nor found in the restored one.
func TestAWriteWhileARestoreHoldsTheDatabaseFails(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	s := stageOver(t, r)
	var attempt error
	tried := false
	s.opts = options{step: func(step string) error {
		if step != "before" {
			return nil
		}
		tried = true
		late, err := sql.Open("sqlite", "file:"+r.DB()+"?_pragma=busy_timeout(50)")
		if err != nil {
			return err
		}
		defer late.Close()
		_, attempt = late.Exec(`UPDATE api_keys SET name = 'written during the move'`)
		return nil
	}}
	done, err := s.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if !tried {
		t.Fatal("it never reached the step, so this tested nothing")
	}
	if attempt == nil {
		t.Error("the write succeeded while the restore held the database")
	}
	for name, path := range map[string]string{"restored": r.DB(), "moved": filepath.Join(done.BeforeDir, "local-base.db")} {
		st, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var count int
		if err := st.DB.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE name = 'written during the move'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		_ = st.Close()
		if count != 0 {
			t.Errorf("the %s database has the write", name)
		}
	}
}

// A restore takes its database out of write-ahead logging with exactly this statement, unless a test set another.
func TestARestoreLeavesWALWithJournalModeDelete(t *testing.T) {
	if got := (options{}).leaveWALStatement(); got != `PRAGMA journal_mode=DELETE` {
		t.Fatalf("a restore leaves write-ahead logging with %q", got)
	}
	if got := (options{leaveWAL: "PRAGMA x"}).leaveWALStatement(); got != "PRAGMA x" {
		t.Fatalf("with a test's statement, a restore uses %q", got)
	}
}

// SQLite answers with the mode it stayed in when it can't change it. A database still in write-ahead logging
// can't be moved safely, so that stops the restore with the root as it was.
func TestARestoreStopsWhenTheDatabaseStaysInWAL(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	s := stageOver(t, r)
	before := tree(t, r.Dir, "runtime")
	s.opts.leaveWAL = `PRAGMA journal_mode` // Answers with the mode it's in, and changes nothing.
	_, err := s.Commit()
	var refused *Refused
	if !errors.As(err, &refused) || err.Error() != someoneElseHoldsIt {
		t.Fatalf("commit: %v", err)
	}
	if after := tree(t, r.Dir, "runtime"); after != before {
		t.Errorf("it stopped, and the root changed:\n%s\nwas:\n%s", after, before)
	}
	if !store.InWAL(r.DB()) {
		t.Error("the database left write-ahead logging, so this tested nothing")
	}
}

// The lock taken for the comparison is kept until the move, which is what locking_mode=EXCLUSIVE is for, so
// nothing writes in between and what was compared is what moves. A write tried there fails and is in neither
// database.
func TestNothingWritesBetweenTheComparisonAndTheMove(t *testing.T) {
	const written = "written between the comparison and the move"
	r, _ := newRoot(t, dir(t))
	s := stageOver(t, r)
	var attempt error
	reached := false
	s.opts = options{step: func(step string) error {
		if step != "switched" {
			return nil
		}
		reached = true
		late, err := sql.Open("sqlite", "file:"+r.DB()+"?_pragma=busy_timeout(0)")
		if err != nil {
			return err
		}
		defer late.Close()
		_, attempt = late.Exec(`UPDATE api_keys SET name = ?`, written)
		return nil
	}}
	done, err := s.Commit()
	if !reached {
		t.Fatalf("it never reached the step, so this tested nothing: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	// Refused because it was locked out: any other failure wouldn't show anything about the lock.
	if !isBusy(attempt) {
		t.Errorf("a write between the comparison and the move: %v", attempt)
	}
	for name, path := range map[string]string{"restored": r.DB(), "moved": filepath.Join(done.BeforeDir, "local-base.db")} {
		st, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var count int
		if err := st.DB.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE name = ?`, written).Scan(&count); err != nil {
			t.Fatal(err)
		}
		_ = st.Close()
		if count != 0 {
			t.Errorf("the %s database has the write", name)
		}
	}
}
