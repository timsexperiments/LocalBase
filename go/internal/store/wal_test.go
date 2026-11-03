package store

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func journalMode(t *testing.T, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}) string {
	t.Helper()
	var mode string
	if err := q.QueryRowContext(context.Background(), `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	return mode
}

// holdConnections checks out n connections at once, so the pool has to open them.
func holdConnections(t *testing.T, db *sql.DB, n int) []*sql.Conn {
	t.Helper()
	conns := make([]*sql.Conn, n)
	for i := range conns {
		c, err := db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		conns[i] = c
	}
	t.Cleanup(func() {
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return conns
}

// Every connection the pool hands out has the same settings, however late it opened.
func TestEveryPooledConnectionHasItsSettings(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i, c := range holdConnections(t, st.DB, maxOpenConns) {
		var timeout, fk, limit int
		ctx := context.Background()
		for pragma, into := range map[string]*int{"busy_timeout": &timeout, "foreign_keys": &fk, "journal_size_limit": &limit} {
			if err := c.QueryRowContext(ctx, `PRAGMA `+pragma).Scan(into); err != nil {
				t.Fatal(err)
			}
		}
		if mode := journalMode(t, c); mode != "wal" || timeout != 5000 || fk != 1 || limit != walSizeLimit {
			t.Errorf("connection %d: mode %s, busy_timeout %d, foreign_keys %d, journal_size_limit %d", i, mode, timeout, fk, limit)
		}
	}
}

// Nothing re-pins the mode: a file switched away stays switched, however many connections open after.
func TestAConnectionOpenedLaterDoesNotChangeTheMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// On the store's own idle connection, the only one attached, as a restore would.
	if _, err := st.DB.ExecContext(context.Background(), `PRAGMA journal_mode=DELETE`); err != nil {
		t.Fatal(err)
	}
	st.DB.SetMaxOpenConns(24) // past the store's own limit, to open more than it normally would
	for i, c := range holdConnections(t, st.DB, 20) {
		if mode := journalMode(t, c); mode != "delete" {
			t.Errorf("connection %d: %s", i, mode)
		}
	}
	if _, err := os.Stat(path + "-wal"); !os.IsNotExist(err) {
		t.Errorf("a -wal is there: %v", err)
	}
}

// A file in rollback mode, as every root is today, converts at its first open and keeps its rows.
func TestOpenConvertsARollbackFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.SetSetting(ctx, "before", "kept"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `PRAGMA journal_mode=DELETE`); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if header := fileHeaderMode(t, path); header != 1 {
		t.Fatalf("not a rollback file to start with: %d", header)
	}
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if mode := journalMode(t, st.DB); mode != "wal" {
		t.Errorf("%s", mode)
	}
	if v, ok, err := st.Setting(ctx, "before"); err != nil || !ok || v != "kept" {
		t.Errorf("%q %v %v", v, ok, err)
	}
}

func fileHeaderMode(t *testing.T, path string) byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil || len(b) < 20 {
		t.Fatalf("%d bytes: %v", len(b), err)
	}
	return b[18]
}

// Whatever mode a file claims when it arrives, a restored one say, the store picks.
func TestAFileCannotChooseTheMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if header := fileHeaderMode(t, path); header != 2 {
		t.Fatalf("the store's own file should say WAL: %d", header)
	}
	for _, claim := range []byte{1, 2} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		b[18], b[19] = claim, claim
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		st, err := Open(path)
		if err != nil {
			t.Fatalf("a file claiming %d: %v", claim, err)
		}
		if mode := journalMode(t, st.DB); mode != "wal" {
			t.Errorf("a file claiming %d: %s", claim, mode)
		}
		st.Close()
	}
}

// While something holds the database exclusively, as a restore does, opening it fails plainly at the
// busy timeout: it doesn't hang, and it doesn't change the mode underneath.
func TestOpeningADatabaseHeldExclusivelyFailsPlainly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	ctx := context.Background()
	holder, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(100)")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	holder.SetMaxOpenConns(1)
	if _, err := holder.ExecContext(ctx, `PRAGMA journal_mode=DELETE`); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.ExecContext(ctx, `PRAGMA locking_mode=EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.ExecContext(ctx, `BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	other, err := Open(path)
	took := time.Since(started)
	if err == nil {
		other.Close()
		t.Fatal("it opened")
	}
	if took > 15*time.Second {
		t.Errorf("took %v", took)
	}
	if !errors.Is(err, ErrInUse) || !strings.HasPrefix(err.Error(), "Another LocalBase process") {
		t.Errorf("%v", err)
	}
	t.Logf("after %v: %v", took.Round(time.Millisecond), err)
	if _, err := holder.ExecContext(ctx, `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	if mode := journalMode(t, holder); mode != "delete" {
		t.Errorf("the mode changed under the holder: %s", mode)
	}
}

// A clean close leaves a database complete in one file.
func TestClosingLeavesOneFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(context.Background(), "k", "v"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
			t.Errorf("%s is left: %v", suffix, err)
		}
	}
}

// The store's descriptors are bounded: past the limit a request waits for a connection, and gets one.
func TestThePoolIsBounded(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if got := st.DB.Stats().MaxOpenConnections; got != maxOpenConns {
		t.Errorf("max open %d, want %d", got, maxOpenConns)
	}
	conns := holdConnections(t, st.DB, maxOpenConns)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := st.DB.Conn(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("one past the limit: %v", err)
	}
	_ = conns[0].Close()
	c, err := st.DB.Conn(context.Background())
	if err != nil {
		t.Fatalf("after one came back: %v", err)
	}
	_ = c.Close()
}

// WriteFailed names what failed, and a store with no log of its own still says so.
func TestWriteFailedIsNeverSilent(t *testing.T) {
	var b strings.Builder
	st := &Store{Log: slog.New(slog.NewJSONHandler(&b, nil))}
	st.WriteFailed("session-touch", errors.New("database is locked"))
	if !strings.Contains(b.String(), `"event":"store.session-touch-write-failed"`) || !strings.Contains(b.String(), "database is locked") {
		t.Errorf("%s", b.String())
	}
	var def strings.Builder
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&def, nil)))
	defer slog.SetDefault(previous)
	(&Store{}).WriteFailed("observed-peak", errors.New("x"))
	if !strings.Contains(def.String(), `"event":"store.observed-peak-write-failed"`) {
		t.Errorf("%s", def.String())
	}
}
