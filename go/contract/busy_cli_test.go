package contract

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
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
	db, err := sql.Open("sqlite", "file:"+filepath.Join(e.root, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`BEGIN EXCLUSIVE`); err != nil {
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
		if !strings.Contains(r.stderr, "is busy") || !strings.Contains(r.stderr, "Try again") {
			t.Errorf("%s doesn't say what to do: %s", c, r.stderr)
		}
		if strings.Contains(r.stderr, " stop") && strings.Contains(r.stderr, "local-base") {
			t.Errorf("%s tells the person to run stop, which disables the service: %s", c, r.stderr)
		}
	}
	// A script gets the same, with a reason it can branch on.
	r := e.cli(nil, "--json", "doctor")
	var out struct {
		Error struct {
			Code, Reason string
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
		t.Fatalf("%s: %v", r.stdout, err)
	}
	if r.code == 0 || out.Error.Reason != "database_busy" {
		t.Errorf("--json doctor: %d %s", r.code, r.stdout)
	}
	if _, err := db.Exec(`ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	for _, c := range commands {
		if r := e.cli(nil, strings.Fields(c)...); r.code != 0 {
			t.Errorf("%s after the writer let go: %d %s%s", c, r.code, r.stdout, r.stderr)
		}
	}
}
