package backup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

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
			if !errors.As(err, &refused) || !strings.Contains(err.Error(), "changed while this waited") || !strings.Contains(err.Error(), c.says) {
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
	shortBusyWaits(t, 0)
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

// The same for an undo that comes before the database moved: a kill between writing the journal and the
// move leaves whatever a process wrote since then, and its writes stay.
func TestAnUndoBeforeTheMoveLeavesWorkingFilesAlone(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	s := stageOver(t, r)
	j := &journal{Version: 1, Before: beforePrefix + "20260101-000000", Had: []string{"local-base.db"}}
	if err := os.WriteFile(r.DB()+"-wal", []byte("someone's writes"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer s.Discard()
	if err := undo(r.Dir, j); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(r.DB() + "-wal"); err != nil || string(got) != "someone's writes" {
		t.Errorf("an undo with nothing moved touched a -wal: %q %v", got, err)
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

// A process that held the database too long stops it, plainly, with nothing changed.
func TestCommitStopsWhenSomethingElseHoldsTheDatabase(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	s := stageOver(t, r)
	shortBusyWaits(t, 0)
	release := holdLock(t, r.DB())
	defer release()
	before := tree(t, r.Dir, "runtime")
	_, err := s.Commit()
	var refused *Refused
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "Something else is using this root's database") {
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
	s := stageOver(t, r)
	late, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer late.Close()
	var n int
	if err := late.DB.QueryRow(`SELECT COUNT(*) FROM api_keys`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	wrote := make(chan error, 1)
	was := stepHook
	defer func() { stepHook = was }()
	started := false
	stepHook = func(step string) error {
		if step == "before" && !started {
			started = true
			go func() {
				_, err := late.DB.Exec(`UPDATE api_keys SET name = 'written during the move'`)
				wrote <- err
			}()
		}
		return nil
	}
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
	was := stepHook
	defer func() { stepHook = was }()
	stepHook = func(step string) error {
		if step == "before" {
			out, err := exec.Command(bin, "--root", r.Dir, "keys", "list").CombinedOutput()
			said, ran = string(out), err
		}
		return nil
	}
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

// TestHelperKilledCommit isn't a test of its own. A real process stages a restore, and is killed at a
// step of Commit, as a person's power cut or kill -9 would.
func TestHelperKilledCommit(t *testing.T) {
	step := os.Getenv("LOCALBASE_TEST_KILL_AT")
	if step == "" {
		t.Skip("only TestAKilledRestoreIsPutBack runs this")
	}
	stepHook = func(at string) error {
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
			before := tree(t, r.Dir, "runtime")
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
			if after := tree(t, r.Dir, "runtime"); after != before {
				t.Errorf("after an undo:\n%s\nwas:\n%s", after, before)
			}
			wholeDatabase(t, r.DB())
		})
	}
}
