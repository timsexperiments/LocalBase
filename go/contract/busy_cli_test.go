package contract

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Every command that reads a root's database, with the real binary, while another process holds it
// past SQLite's wait. What a person gets is a plain message and a failure, never a raw SQLite error,
// and never a report that exits 0 on settings it couldn't read.
func TestCommandsOnABusyRootSaySo(t *testing.T) {
	backupGoOnly(t)
	tmp, _ := filepath.EvalSymlinks(scratchDir(t))
	e := backupRoot(t, tmp, "root")
	commands := []string{"status", "usage", "keys list", "models list", "doctor"}
	for _, c := range commands {
		if r := e.cli(nil, strings.Fields(c)...); r.code != 0 {
			t.Fatalf("%s on a quiet root: %d %s%s", c, r.code, r.stdout, r.stderr)
		}
	}
	// Under write-ahead logging a writer doesn't make a reader busy. Another connection rebuilding the
	// WAL index does, as a crashed root's first opener does beside a second process: this process
	// stays attached, so the -shm stays, then holds its write and recovery locks (bytes 120 and 122)
	// and spoils the first copy of the index header. Each command, a process of its own, then waits
	// out its busy timeout and gets SQLITE_BUSY_RECOVERY. Found by Reliability.
	dbPath := filepath.Join(e.root, "local-base.db")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var tables int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	shm, err := os.OpenFile(dbPath+"-shm", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []int64{120, 122} {
		lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: io.SeekStart, Start: b, Len: 1}
		if err := syscall.FcntlFlock(shm.Fd(), syscall.F_SETLK, &lock); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := shm.WriteAt(bytes.Repeat([]byte{0xFF}, 48), 0); err != nil {
		t.Fatal(err)
	}
	for _, c := range commands {
		start := time.Now()
		r := e.cli(nil, strings.Fields(c)...)
		said := r.stdout + r.stderr
		t.Logf("%-12s exit %d in %.1fs: %.160s", c, r.code, time.Since(start).Seconds(), strings.ReplaceAll(r.stderr, "\n", " "))
		if r.code == 0 {
			t.Errorf("%s exits 0 on a database it couldn't read", c)
		}
		if strings.Contains(said, "SQLITE_BUSY") || strings.Contains(said, "database is locked") {
			t.Errorf("%s shows the raw error: %s", c, said)
		}
		// A command that opens the store to change it is refused at the open, in #613's approved words; one
		// that reads gets #544's. Both say to wait or stop what holds it.
		openRefused := strings.Contains(r.stderr, "Another LocalBase process is using this root. Stop it or wait, then try again.")
		if !openRefused && (!strings.Contains(r.stderr, "is busy") || !strings.Contains(r.stderr, "Try again")) {
			t.Errorf("%s doesn't say what to do: %s", c, r.stderr)
		}
		if strings.Contains(r.stderr, " stop") && strings.Contains(r.stderr, "local-base") {
			t.Errorf("%s tells the person to run stop, which disables the service: %s", c, r.stderr)
		}
	}
	// A script gets the same, with a reason it can branch on.
	r := e.cli(nil, "--json", "doctor")
	// doctor reports the database and carries on, so this is its data, with a failing exit.
	var out struct {
		Data struct {
			DatabaseProblem struct {
				Message, Reason string
			} `json:"databaseProblem"`
			Hardware any `json:"hardware"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
		t.Fatalf("%s: %v", r.stdout, err)
	}
	if r.code == 0 || out.Data.DatabaseProblem.Reason != "database_busy" || out.Data.Hardware == nil {
		t.Errorf("--json doctor: %d %s", r.code, r.stdout)
	}
	_ = shm.Close() // lets go of the locks; the next reader rebuilds the index from the -wal
	for _, c := range commands {
		if r := e.cli(nil, strings.Fields(c)...); r.code != 0 {
			t.Errorf("%s after the writer let go: %d %s%s", c, r.code, r.stdout, r.stderr)
		}
	}
}
