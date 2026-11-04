package contract

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/backup"
)

// lockedBuffer is written by a process's copier and read by the test.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// onATerminal runs a command with a terminal as its stdin, because restore only asks on one. script(1)
// gives one with no new dependency, and macOS and Linux spell it differently.
func onATerminal(path string, args ...string) *exec.Cmd {
	if runtime.GOOS == "darwin" {
		return exec.Command("script", append([]string{"-q", "/dev/null", path}, args...)...)
	}
	quoted := []string{"'" + path + "'"}
	for _, a := range args {
		quoted = append(quoted, "'"+strings.ReplaceAll(a, "'", `'\''`)+"'")
	}
	return exec.Command("script", "-q", "-e", "-c", strings.Join(quoted, " "), "/dev/null")
}

// waitingRestore is a real restore process, stopped at its question.
type waitingRestore struct {
	t        *testing.T
	said     lockedBuffer
	answer   io.WriteCloser
	finished chan error
}

func askingRestore(t *testing.T, e *e2e, file string) *waitingRestore {
	t.Helper()
	cmd := onATerminal(goCLI(t), "--root", e.root, "restore", "--replace", "--no-port-probe", file)
	cmd.Env = append(cleanEnv(), e.env...)
	w := &waitingRestore{t: t, finished: make(chan error, 1)}
	var err error
	if w.answer, err = cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = &w.said, &w.said
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { w.finished <- cmd.Wait() }()
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
		}
	})
	deadline := time.Now().Add(60 * time.Second)
	for !strings.Contains(w.said.String(), "Continue? [y/N]") {
		if time.Now().After(deadline) {
			t.Fatalf("restore never asked:\n%s", w.said.String())
		}
		select {
		case err := <-w.finished:
			t.Fatalf("restore ended before it asked (%v):\n%s", err, w.said.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
	return w
}

// yes answers, and says how it ended.
func (w *waitingRestore) yes() (said string, failed bool) {
	w.t.Helper()
	if _, err := io.WriteString(w.answer, "y\n"); err != nil {
		w.t.Fatal(err)
	}
	select {
	case err := <-w.finished:
		// script(1) passes on the exit status on Linux with -e, and on macOS by itself.
		return w.said.String(), err != nil
	case <-time.After(60 * time.Second):
		w.t.Fatalf("restore didn't finish:\n%s", w.said.String())
	}
	return "", false
}

// rootFiles hashes everything in a root but what a process keeps while it runs.
func rootFiles(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			if rel == "runtime" || rel == "logs" {
				return filepath.SkipDir
			}
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf("%s %x", rel, sha256.Sum256(b)))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

// A restore's report is what someone says yes to. If the root changed while it waited, that yes was to
// something else, so the restore stops with nothing changed, whatever the change was. This is the real
// command path, in two processes: restore waits at its question, the change is made, restore is answered.
func TestARootThatChangedWhileRestoreAskedStopsIt(t *testing.T) {
	backupGoOnly(t)
	for _, c := range []struct {
		name string
		says string
		// change makes the change, with a key the backup holds.
		change func(t *testing.T, e *e2e, id string)
	}{
		{"a key revoked", "API keys", func(t *testing.T, e *e2e, id string) {
			if r := e.cli(nil, "keys", "revoke", id); r.code != 0 {
				t.Fatalf("revoke: %s%s", r.stdout, r.stderr)
			}
		}},
		{"a key created", "API keys", func(t *testing.T, e *e2e, id string) {
			if r := e.cli(nil, "keys", "create", "--name", "made-while-it-asked"); r.code != 0 {
				t.Fatalf("create: %s%s", r.stdout, r.stderr)
			}
		}},
		{"a key rotated", "API keys", func(t *testing.T, e *e2e, id string) {
			if r := e.cli(nil, "keys", "rotate", id); r.code != 0 {
				t.Fatalf("rotate: %s%s", r.stdout, r.stderr)
			}
		}},
		// No command disables a user: that's a route, and a restore refuses a running gateway. So this
		// is a separate connection, which is what a route would be.
		{"a user disabled", "users", func(t *testing.T, e *e2e, id string) {
			db, err := sql.Open("sqlite", "file:"+filepath.Join(e.root, "local-base.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`UPDATE auth_users SET status = 'disabled' WHERE id = 'a-person'`); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			tmp, _ := filepath.EvalSymlinks(scratchDir(t))
			e := backupRoot(t, tmp, "root")
			db, err := sql.Open("sqlite", "file:"+filepath.Join(e.root, "local-base.db"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO auth_users (id, status, created_at, updated_at) VALUES ('a-person', 'active', 'x', 'x')`); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			created := e.json("keys", "create", "--name", "asked", "--scopes", "models:read")
			id := created["key"].(map[string]any)["id"].(string)
			file := filepath.Join(tmp, "b"+backup.Extension)
			if r := e.cli(nil, "backup", "--output", file); r.code != 0 {
				t.Fatalf("backup: %s%s", r.stdout, r.stderr)
			}

			restore := askingRestore(t, e, file)
			c.change(t, e, id)
			before := rootFiles(t, e.root)
			said, failed := restore.yes()
			if !failed || !strings.Contains(said, "changed while this waited") || !strings.Contains(said, c.says) {
				t.Fatalf("it should have stopped and named %q: %v\n%s", c.says, failed, said)
			}

			// Nothing changed is proved, not said: the files, the journal, and everything that uses them.
			if after := rootFiles(t, e.root); after != before {
				t.Errorf("it stopped, and the root changed:\n%s\nwas:\n%s", after, before)
			}
			if _, err := os.Lstat(filepath.Join(e.root, ".restore-journal.json")); err == nil {
				t.Error("it stopped, and left its journal")
			}
			if r := e.cli(nil, "keys", "list"); r.code != 0 {
				t.Errorf("keys list after it stopped: %s%s", r.stdout, r.stderr)
			}
			if r := e.cli(nil, "restore", "--undo"); r.code == 0 || !strings.Contains(r.stdout+r.stderr, "no unfinished restore") {
				t.Errorf("--undo after it stopped: %d %s%s", r.code, r.stdout, r.stderr)
			}
			e.serve()
			defer e.stop()
			if r := e.do("GET", "/health", ""); r.Status != 200 {
				t.Errorf("a gateway after it stopped: %d", r.Status)
			}
		})
	}
}

// The control: with nothing changed, the same steps restore, and a key revoked before it asked is kept.
func TestARootThatDidntChangeIsRestored(t *testing.T) {
	backupGoOnly(t)
	tmp, _ := filepath.EvalSymlinks(scratchDir(t))
	e := backupRoot(t, tmp, "root")
	created := e.json("keys", "create", "--name", "asked", "--scopes", "models:read")
	leaked := created["secret"].(string)
	id := created["key"].(map[string]any)["id"].(string)
	file := filepath.Join(tmp, "b"+backup.Extension)
	if r := e.cli(nil, "backup", "--output", file); r.code != 0 {
		t.Fatalf("backup: %s%s", r.stdout, r.stderr)
	}
	if r := e.cli(nil, "keys", "revoke", id); r.code != 0 {
		t.Fatalf("revoke: %s%s", r.stdout, r.stderr)
	}
	said, failed := askingRestore(t, e, file).yes()
	if failed || !strings.Contains(said, "Restored to") {
		t.Fatalf("it should have restored: %v\n%s", failed, said)
	}
	e.serve()
	defer e.stop()
	control := e.do("GET", "/v1/models", "")
	if control.Status != 200 {
		t.Fatalf("the control: a key nobody revoked gets %d: %s", control.Status, control.Body)
	}
	e.key = leaked
	if r := e.do("GET", "/v1/models", ""); r.Status != 401 {
		t.Errorf("a key revoked before it asked gets %d after the restore", r.Status)
	}
}
