// Package store owns the LocalBase SQLite database.
//
// The schema and its migration journal are shared with the TypeScript
// implementation: the same drizzle SQL files are embedded and applied, and
// the `__drizzle_migrations` rows are byte-identical (sha256 of each file and
// the journal timestamp). Either binary can therefore open a database the
// other created, which is what makes rollback a binary swap.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

//go:embed migrations/*.sql migrations/meta/_journal.json
var migrationFS embed.FS

type Store struct {
	DB *sql.DB
	// Log is where WriteFailed reports. Set it before anything can write in the background.
	Log *slog.Logger
	// stage is a private copy being read, if there is one: see readablePath.
	stage string
}

type migration struct {
	tag       string
	hash      string
	createdAt int64
	sql       string
}

func loadMigrations() ([]migration, error) {
	raw, err := migrationFS.ReadFile("migrations/meta/_journal.json")
	if err != nil {
		return nil, err
	}
	var journal struct {
		Entries []struct {
			Tag  string `json:"tag"`
			When int64  `json:"when"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &journal); err != nil {
		return nil, err
	}
	out := make([]migration, 0, len(journal.Entries))
	for _, e := range journal.Entries {
		body, err := migrationFS.ReadFile("migrations/" + e.Tag + ".sql")
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		out = append(out, migration{tag: e.Tag, hash: hex.EncodeToString(sum[:]), createdAt: e.When, sql: string(body)})
	}
	return out, nil
}

// Open opens (creating if needed) the database and applies pending migrations.
func Open(path string) (*Store, error) {
	makePrivate(path)
	// busy_timeout: the gateway and CLI share this file; wait for another
	// process's write lock instead of failing with SQLITE_BUSY (#359).
	// _txlock=immediate: writes never hit the deferred read-to-write upgrade,
	// which returns SQLITE_BUSY without consulting the busy handler.
	// journal_size_limit: what a checkpoint leaves of the -wal file, so a burst doesn't keep its size.
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_size_limit(" + strconv.Itoa(walSizeLimit) + ")&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(MaxOpenConns)
	db.SetMaxIdleConns(MaxIdleConns)
	s := &Store{DB: db}
	if err := checkWALBelongs(path); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.useWAL(context.Background()); err != nil {
		db.Close()
		if readOnlyRoot(err) {
			return OpenReadOnly(path) // nothing here can be written, and what it reads is still good
		}
		return nil, plainIfBusy(err)
	}
	recordWALOwner(path)
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		if readOnlyRoot(err) {
			return OpenReadOnly(path)
		}
		return nil, plainIfBusy(err)
	}
	return s, nil
}

// ErrInUse: something else held the database for longer than the busy timeout while it was opened.
var ErrInUse = errors.New("Another LocalBase process is using this root. Stop it or wait, then try again.")

// plainIfBusy says what a person can do about a busy database, and keeps the cause for the log.
func plainIfBusy(err error) error {
	var e *sqlite.Error
	if errors.As(err, &e) && e.Code()&0xff == sqlite3.SQLITE_BUSY {
		return fmt.Errorf("%w", ErrInUse)
	}
	return err
}

// WriteFailed says that a write nobody is waiting on failed, so it isn't lost silently. what is a word
// from the code. It logs to the store's Log, or to the default logger for a command that set none.
func (s *Store) WriteFailed(what string, err error) {
	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	log.Warn("A background database write failed.", "event", "store."+what+"-write-failed", "error", err.Error())
}

// walSizeLimit is the most of a -wal file a checkpoint leaves behind, in bytes.
const walSizeLimit = 64 << 20

// useWAL puts the database in write-ahead logging, so readers and a writer don't wait for each other.
// The mode is stored in the file, so this is the one place that sets it, whatever a file says when it
// arrives, and it isn't in the DSN: that would run on every new connection, and flip a file back
// while something that has switched it away, such as a restore, is working. A process already running
// doesn't notice a change of mode, which is safe because a restore only switches with nothing attached.
func (s *Store) useWAL(ctx context.Context) error {
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }() // back to the pool; the mode is already set or the error is
	var mode string
	if err := conn.QueryRowContext(ctx, `PRAGMA journal_mode=WAL`).Scan(&mode); err != nil {
		return fmt.Errorf("could not set the database's journal mode: %w", err)
	}
	if mode != "wal" {
		return fmt.Errorf("could not set the database's journal mode: it stayed %q", mode)
	}
	return nil
}

// makePrivate creates the database file 0600 if it is missing, since SQLite would
// create it under the umask and it holds secrets (DESIGN §27), and tightens the file
// and its journals if they exist. SQLite gives new journals the database's mode. An
// empty file is a new database to SQLite. It never fails: root.TightenPermissions
// reports what could not be changed, and SQLite reports what cannot be opened.
func makePrivate(path string) {
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err == nil {
		f.Close()
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		p := path + suffix
		if info, err := os.Lstat(p); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o077 != 0 {
			_ = os.Chmod(p, info.Mode().Perm()&^0o077) // serve's TightenPermissions reports what stays loose
		}
	}
}

// OpenReadOnly opens an existing database without migrating or writing it.
func OpenReadOnly(path string) (*Store, error) {
	if err := checkWALBelongs(path); err != nil {
		return nil, err
	}
	stage, read, err := readablePath(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+read+"?mode=ro&_pragma=busy_timeout(5000)")
	if err == nil {
		// Ping doesn't read the file, and a database that can't be read should fail here.
		err = db.QueryRow(`SELECT count(*) FROM sqlite_master`).Err()
		if err != nil {
			db.Close()
		}
	}
	if err != nil {
		removeStage(stage)
		return nil, err
	}
	return &Store{DB: db, stage: stage}, nil
}

func (s *Store) Close() error {
	err := s.DB.Close()
	removeStage(s.stage)
	return err
}

// drizzleTable is the migration journal, which the TS build also makes.
const drizzleTable = `CREATE TABLE IF NOT EXISTS "__drizzle_migrations" (
		id SERIAL PRIMARY KEY,
		hash text NOT NULL,
		created_at numeric
	)`

func (s *Store) migrate(ctx context.Context) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, drizzleTable); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT hash, created_at FROM "__drizzle_migrations" ORDER BY created_at`)
	if err != nil {
		return err
	}
	type applied struct {
		hash      string
		createdAt int64
	}
	var done []applied
	for rows.Next() {
		var a applied
		if err := rows.Scan(&a.hash, &a.createdAt); err != nil {
			rows.Close()
			return err
		}
		done = append(done, a)
	}
	rows.Close()
	if len(done) > len(migrations) {
		return fmt.Errorf("database has %d migrations but this build knows %d; a newer LocalBase wrote it", len(done), len(migrations))
	}
	for i, a := range done {
		if a.hash != migrations[i].hash || a.createdAt != migrations[i].createdAt {
			return fmt.Errorf("migration journal mismatch at %s: hashes or order differ from this build", migrations[i].tag)
		}
	}
	for _, m := range migrations[len(done):] {
		for _, stmt := range strings.Split(m.sql, "--> statement-breakpoint") {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("migration %s: %w", m.tag, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO "__drizzle_migrations" ("hash", "created_at") VALUES (?, ?)`, m.hash, m.createdAt); err != nil {
			return err
		}
	}
	if err := applyGoSchema(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}
