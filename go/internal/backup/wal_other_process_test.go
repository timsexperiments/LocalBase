package backup

import (
	"bufio"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// A connection in this process can't show what a restore meets from another one, because SQLite shares its
// locks within a process, and POSIX ones belong to the process. These use a real second process, opened the
// way every command opens a database.

const (
	idleStoreEnv = "LOCALBASE_TEST_IDLE_STORE"
	tryLockEnv   = "LOCALBASE_TEST_TRY_LOCK"
)

// TestHelperIdleStore isn't a test of its own. idleStore runs it as a process that has opened a database and
// is then doing nothing until told to write.
func TestHelperIdleStore(t *testing.T) {
	path := os.Getenv(idleStoreEnv)
	if path == "" {
		t.Skip("run by idleStore")
	}
	st, err := store.Open(path)
	if err != nil {
		fmt.Println("failed:", err)
		return
	}
	defer st.Close()
	var n int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM api_keys`).Scan(&n); err != nil {
		fmt.Println("failed:", err)
		return
	}
	fmt.Println("ready")
	lines := bufio.NewScanner(os.Stdin)
	for lines.Scan() {
		kind, value, _ := strings.Cut(lines.Text(), " ")
		if kind == "key" {
			_, err = st.DB.Exec(`UPDATE api_keys SET name = ?`, value)
		} else {
			_, err = st.DB.Exec(`CREATE TABLE IF NOT EXISTS written (value TEXT)`)
			if err == nil {
				_, err = st.DB.Exec(`INSERT INTO written VALUES (?)`, value)
			}
		}
		fmt.Println("wrote:", err)
	}
}

// TestHelperTriesForTheLock isn't a test of its own. triesForTheLock runs it as a process asking for the lock
// a restore takes, without waiting.
func TestHelperTriesForTheLock(t *testing.T) {
	path := os.Getenv(tryLockEnv)
	if path == "" {
		t.Skip("run by triesForTheLock")
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=rw&_pragma=busy_timeout(0)")
	if err != nil {
		fmt.Println("result:", err)
		return
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var mode string
	if err := db.QueryRow(`PRAGMA locking_mode=EXCLUSIVE`).Scan(&mode); err != nil {
		fmt.Println("result:", err)
		return
	}
	_, err = db.Exec(`BEGIN EXCLUSIVE`)
	fmt.Println("result:", err)
}

func triesForTheLock(t *testing.T, dbPath string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperTriesForTheLock$")
	cmd.Env = append(os.Environ(), tryLockEnv+"="+dbPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the other process: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "result: ") {
			return strings.TrimPrefix(line, "result: ")
		}
	}
	t.Fatalf("the other process said nothing: %s", out)
	return ""
}

type idleProcess struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *bufio.Reader
	gone bool
}

func idleStore(t *testing.T, dbPath string) *idleProcess {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperIdleStore$")
	cmd.Env = append(os.Environ(), idleStoreEnv+"="+dbPath)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &idleProcess{cmd: cmd, in: in, out: bufio.NewReader(out)}
	t.Cleanup(p.kill)
	if line := p.line(t); line != "ready" {
		t.Fatalf("the other process: %s", line)
	}
	return p
}

func (p *idleProcess) line(t *testing.T) string {
	t.Helper()
	line, err := p.out.ReadString('\n')
	if err != nil {
		t.Fatalf("the other process: %q %v", line, err)
	}
	return strings.TrimSpace(line)
}

// write has the other process write, and says what it was told.
func (p *idleProcess) write(t *testing.T, kind, value string) string {
	t.Helper()
	if _, err := fmt.Fprintln(p.in, kind, value); err != nil {
		t.Fatal(err)
	}
	return p.line(t)
}

// stop lets it close its store and exit.
func (p *idleProcess) stop(t *testing.T) {
	t.Helper()
	if p.gone {
		return
	}
	p.gone = true
	_ = p.in.Close()
	_, _ = io.Copy(io.Discard, p.out)
	if err := p.cmd.Wait(); err != nil {
		t.Errorf("the other process: %v", err)
	}
}

// kill ends it with no chance to close anything.
func (p *idleProcess) kill() {
	if p.gone {
		return
	}
	p.gone = true
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
}

func keysNamed(t *testing.T, dbPath, name string) int {
	t.Helper()
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var n int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func rowsWritten(t *testing.T, st *store.Store, value string) int {
	t.Helper()
	var n int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM written WHERE value = ?`, value).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A process with the database open stops a restore, however idle it is, and what it writes afterwards is kept.
func TestAnIdleProcessWithTheDatabaseOpenStopsARestore(t *testing.T) {
	const written = "written after it stopped"
	r, _ := newRoot(t, dir(t))
	s := stageOver(t, r)
	s.opts = shortBusyWaits(200 * time.Millisecond)
	other := idleStore(t, r.DB())
	_, err := s.Commit()
	var refused *Refused
	if !errors.As(err, &refused) || err.Error() != someoneElseHoldsIt {
		t.Fatalf("commit, with another process attached: %v", err)
	}
	if exists(journalPath(r.Dir)) {
		t.Error("it stopped, and left its journal")
	}
	if said := other.write(t, "key", written); said != "wrote: <nil>" {
		t.Errorf("the other process's write: %s", said)
	}
	other.stop(t)
	if keysNamed(t, r.DB(), written) == 0 {
		t.Error("its write isn't there, with that process gone")
	}
}

// Whatever a restore does about a process attached to the database, that process's write can't report success
// and then be gone. If the restore stops, the write is kept. If it went ahead, a write that reported success
// has to be in the moved database. It's read back with that process gone, since its own close could write it.
func TestAWriteFromAnotherProcessIsNeverReportedAndLost(t *testing.T) {
	const written = "written during the move"
	r, _ := newRoot(t, dir(t))
	s := stageOver(t, r)
	s.opts = shortBusyWaits(200 * time.Millisecond)
	other := idleStore(t, r.DB())
	said := ""
	s.opts.step = func(step string) error {
		if step == "before" {
			said = other.write(t, "key", written)
		}
		return nil
	}
	done, err := s.Commit()
	if err != nil {
		if err.Error() != someoneElseHoldsIt {
			t.Fatalf("commit: %v", err)
		}
		if said := other.write(t, "key", written); said != "wrote: <nil>" {
			t.Errorf("after the stop, the other process's write: %s", said)
		}
		other.stop(t)
		if keysNamed(t, r.DB(), written) == 0 {
			t.Error("after the stop, its write isn't there")
		}
		return
	}
	other.stop(t)
	restored, moved := keysNamed(t, r.DB(), written), keysNamed(t, filepath.Join(done.BeforeDir, "local-base.db"), written)
	if restored != 0 {
		t.Error("the write went into the restored database")
	}
	if said == "wrote: <nil>" && moved == 0 {
		t.Error("the write reported success and is in neither database")
	}
}

// A stop's close must not count as the last connection beside a process that's attached: that would delete the
// process's -wal while saying nothing was changed. What it wrote stays however it ends, and beside a third
// process that writes before it does.
func TestAStopBesideAnAttachedProcessLeavesItsWrites(t *testing.T) {
	for _, ends := range []string{"closes", "is killed", "closes after a third process writes"} {
		t.Run(ends, func(t *testing.T) {
			r, _ := newRoot(t, dir(t))
			s := stageOver(t, r)
			s.opts = shortBusyWaits(200 * time.Millisecond)
			other := idleStore(t, r.DB())
			if said := other.write(t, "key", "changed by the other process"); said != "wrote: <nil>" {
				t.Fatalf("its first write: %s", said)
			}
			if info, err := os.Stat(r.DB() + "-wal"); err != nil || info.Size() == 0 {
				t.Fatalf("no -wal, so this tests nothing: %v", err)
			}
			_, err := s.Commit()
			var refused *Refused
			if !errors.As(err, &refused) || err.Error() != someoneElseHoldsIt {
				t.Fatalf("commit, with another process attached: %v", err)
			}
			if info, err := os.Stat(r.DB() + "-wal"); err != nil || info.Size() == 0 {
				t.Errorf("the stop took the other process's -wal: %v", err)
			}
			if said := other.write(t, "row", "first"); said != "wrote: <nil>" {
				t.Errorf("its write after the stop: %s", said)
			}
			third := ends == "closes after a third process writes"
			switch {
			case third:
				another := idleStore(t, r.DB())
				if said := another.write(t, "row", "second"); said != "wrote: <nil>" {
					t.Errorf("the third process's write: %s", said)
				}
				other.stop(t)
				another.stop(t)
			case ends == "is killed":
				other.kill()
			default:
				other.stop(t)
			}
			st, err := store.Open(r.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			wholeOf(t, st.DB)
			if rowsWritten(t, st, "first") != 1 {
				t.Error("its write after the stop isn't there")
			}
			if third && rowsWritten(t, st, "second") != 1 {
				t.Error("the third process's write isn't there")
			}
		})
	}
}

// Closing any descriptor on a file drops this process's locks on it, so a raw read of the database while a
// restore holds it mustn't cost the restore its lock. Nothing does that today, and this keeps it safe if
// something does.
func TestARawReadDuringTheHoldKeepsTheLock(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	s := stageOver(t, r)
	got, reached := "", false
	s.opts = options{step: func(step string) error {
		if step == "before" {
			reached = true
			store.InWAL(r.DB())
			got = triesForTheLock(t, r.DB())
		}
		return nil
	}}
	if _, err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	if !reached {
		t.Fatal("it never reached the step, so this tested nothing")
	}
	if !strings.Contains(got, "locked") && !strings.Contains(got, "busy") {
		t.Errorf("another process, asking for the lock during the hold after a raw read: %s", got)
	}
}
