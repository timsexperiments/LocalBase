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
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql migrations/meta/_journal.json
var migrationFS embed.FS

type Store struct {
	DB *sql.DB
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
	// busy_timeout: the gateway and CLI share this file; wait for another
	// process's write lock instead of failing with SQLITE_BUSY (#359).
	// _txlock=immediate: writes never hit the deferred read-to-write upgrade,
	// which returns SQLITE_BUSY without consulting the busy handler.
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{DB: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.DB.Close() }

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
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS "__drizzle_migrations" (
		id SERIAL PRIMARY KEY,
		hash text NOT NULL,
		created_at numeric
	)`); err != nil {
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
	return tx.Commit()
}
