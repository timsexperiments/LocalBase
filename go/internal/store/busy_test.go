package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func TestIsBusyKnowsALockedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-base.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	writer, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	writer.SetMaxOpenConns(1)
	if _, err := writer.Exec(`BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	reader, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var n int
	readErr := reader.QueryRow(`SELECT COUNT(*) FROM sqlite_master`).Scan(&n)
	if !IsBusy(readErr) {
		t.Fatalf("a locked database: %v", readErr)
	}
	if !IsBusy(fmt.Errorf("reading the settings: %w", readErr)) {
		t.Error("a wrapped busy error isn't recognized")
	}
	if !IsBusy(&BusyError{readErr}) {
		t.Error("BusyError should keep the failure underneath")
	}
	said := (&BusyError{readErr}).Error()
	if !strings.Contains(said, "is busy") || strings.Contains(said, "SQLITE_BUSY") || strings.Contains(said, "locked") {
		t.Errorf("says: %s", said)
	}
	if _, err := writer.Exec(`ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Exec(`SELECT * FROM no_such_table`); err == nil || IsBusy(err) {
		t.Errorf("a bad query isn't a busy database: %v", err)
	}
	if IsBusy(errors.New("database is locked")) || IsBusy(nil) {
		t.Error("only SQLite's own error counts")
	}
}

// SQLITE_LOCKED is the other way a database can be held: a table in use by the same connection.
// Nothing here does that on purpose, and this is so that nothing could bring back the raw error.
func TestIsBusyKnowsALockedTable(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "locked.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `CREATE TABLE t (x); INSERT INTO t VALUES (1), (2), (3)`); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.QueryContext(ctx, `SELECT x FROM t`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no rows")
	}
	_, dropErr := conn.ExecContext(ctx, `DROP TABLE t`)
	var e *sqlite.Error
	if !errors.As(dropErr, &e) || e.Code()&0xff != sqlite3.SQLITE_LOCKED {
		t.Fatalf("this didn't produce SQLITE_LOCKED, so it proves nothing: %v", dropErr)
	}
	if !IsBusy(dropErr) {
		t.Errorf("IsBusy: %v", dropErr)
	}
}
