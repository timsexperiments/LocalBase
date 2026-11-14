package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// checkDatabase judges the file before anything runs against it: read-only, with the schema's
// functions untrusted, and before any migration. integrity_check alone would pass a trigger that
// adds a role binding after every policy change, and no report would show it.
func checkDatabase(ctx context.Context, dbPath string, o options) error {
	db, err := openReadOnly(dbPath, o)
	if err != nil {
		return refuse("The database can't be opened: %v", err)
	}
	defer db.Close()
	var result string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check(1)`).Scan(&result); err != nil {
		return refuse("The database is damaged: %v", err)
	}
	if result != "ok" {
		return refuse("The database is damaged: %s", result)
	}
	level, err := migrationLevel(ctx, db)
	if err != nil {
		return refuse("The database can't be read: %v", err)
	}
	known, err := store.MigrationCount()
	if err != nil {
		return err
	}
	if level > known {
		return refuse("A newer LocalBase wrote this backup: its database has %d migrations, and this version knows %d. Upgrade LocalBase to restore it.", level, known)
	}
	schema, err := tableExists(ctx, db, "go_schema")
	if err != nil {
		return refuse("The database can't be read: %v", err)
	}
	if schema {
		var v int
		if err := db.QueryRowContext(ctx, `SELECT version FROM go_schema WHERE id = 1`).Scan(&v); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return refuse("The database can't be read: %v", err)
		}
		if v > store.GoSchemaVersion() {
			return refuse("A newer LocalBase wrote this backup: its database schema is %d, and this version knows %d. Upgrade LocalBase to restore it.", v, store.GoSchemaVersion())
		}
	}
	shape, err := store.ReadShape(ctx, db)
	if err != nil {
		return refuse("The database can't be read: %v", err)
	}
	expected, err := store.ExpectedShape(ctx, level)
	if err != nil {
		return err
	}
	if bad := shape.Unexpected(expected); len(bad) > 0 {
		return refuse("The database holds what LocalBase does not make, so it was changed by something else: %s.", strings.Join(bad, "; "))
	}
	if err := checkDatabaseSize(ctx, db, shape); err != nil {
		return err
	}
	return checkForeignKeys(ctx, db)
}

// checkDatabaseSize refuses a database holding more than a root could, before anything reads it into
// memory. octet_length takes a value's size from its header, so a large value isn't read to find out
// that it's large.
func checkDatabaseSize(ctx context.Context, db *sql.DB, shape *store.Shape) error {
	for _, name := range slices.Sorted(maps.Keys(shape.Tables)) {
		table := shape.Tables[name]
		query := `SELECT COUNT(*)`
		for _, column := range table.Columns {
			query += `, COALESCE(MAX(octet_length(` + quoteName(column.Name) + `)), 0), COALESCE(SUM(octet_length(` + quoteName(column.Name) + `)), 0)`
		}
		values := make([]int64, 1+2*len(table.Columns))
		targets := make([]any, len(values))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := db.QueryRowContext(ctx, query+` FROM `+quoteName(name)).Scan(targets...); err != nil {
			return refuse("The database can't be read: %v", err)
		}
		var total int64
		for i := range table.Columns {
			if largest := values[1+2*i]; largest > maxDatabaseValueBytes {
				return refuse("The database holds more than a root could, so it was changed by something else: a value of %d bytes in %s.", largest, name)
			}
			total += values[2+2*i]
		}
		if name == usageTable {
			continue
		}
		if values[0] > maxDatabaseRows {
			return refuse("The database holds more than a root could, so it was changed by something else: %d rows in %s.", values[0], name)
		}
		if total > maxDatabaseTableBytes {
			return refuse("The database holds more than a root could, so it was changed by something else: %d bytes in %s.", total, name)
		}
	}
	return nil
}

// settleJournalMode leaves the staged database as one file. How a database is journaled is the store's
// to decide, so a file doesn't get to choose it for the root by what its header claims, and nothing
// beside the database has to move with it. It's the last thing done to the staged database, so that
// whatever has been written is already in there.
func settleJournalMode(ctx context.Context, db *sql.DB) error {
	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode=DELETE`).Scan(&mode); err != nil {
		return err
	}
	return modeAfterSettling(mode)
}

// journalModes are SQLite's own words for a journal mode, the only ones a refusal shows.
var journalModes = []string{"delete", "truncate", "persist", "memory", "wal", "off"}

// modeStayedError is a journal mode that didn't change. Its mode is always one of journalModes.
type modeStayedError struct{ mode string }

func (e *modeStayedError) Error() string { return "its journal mode stayed " + e.mode }

// modeAfterSettling says what SQLite's answer means. An answer that isn't one of its own words, which SQLite
// doesn't give, is said without it.
func modeAfterSettling(mode string) error {
	mode = strings.ToLower(mode)
	switch {
	case mode == "delete":
		return nil
	case slices.Contains(journalModes, mode):
		return &modeStayedError{mode}
	}
	return errors.New("its journal mode didn't change")
}

// usageTable piles up, and a restore never reads it into memory, so only its values are limited.
const usageTable = "usage_hourly"

func quoteName(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// checkForeignKeys refuses a database with rows pointing at something that isn't there. LocalBase keeps
// foreign keys on, so only an edit of the file can make one, and what reads such a row may not look
// at what it points to. It runs after the schema is compared, so the foreign keys it checks are the
// ones this version makes, and the tables it names are too. It says how many rows, and nothing in them.
func checkForeignKeys(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return refuse("The database can't be read: %v", err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var key int
		if err := rows.Scan(&table, &rowid, &parent, &key); err != nil {
			return refuse("The database can't be read: %v", err)
		}
		counts[table]++
	}
	if err := rows.Err(); err != nil {
		return refuse("The database can't be read: %v", err)
	}
	if len(counts) == 0 {
		return nil
	}
	var found []string
	for table, n := range counts {
		found = append(found, fmt.Sprintf("%s: %d", table, n))
	}
	slices.Sort(found)
	return refuse("The database has rows pointing at something that isn't there, so it was changed by something else. %s.", strings.Join(found, ", "))
}
