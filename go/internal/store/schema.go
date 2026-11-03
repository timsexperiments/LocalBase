package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// A backup is input from someone else (DESIGN §34.5), so its database's shape is compared
// with what this build creates. The shape comes from SQLite's own view of each table, not
// the text it was created with: the TS build and this one write the same definitions with
// different quoting and spacing, and a false refusal would make a real backup useless.

// Column is what pragma_table_xinfo says about a column. Hidden is non-zero for a
// generated column.
type Column struct {
	Name, Type, Default string
	NotNull             bool
	PrimaryKey, Hidden  int
}

// Index is what pragma_index_list and pragma_index_info say about an index.
type Index struct {
	Name, Origin string
	Unique       bool
	Partial      bool
	Columns      []string
}

// ForeignKey is one row of pragma_foreign_key_list.
type ForeignKey struct{ Table, From, To, OnUpdate, OnDelete string }

// Table is one table's shape.
type Table struct {
	Columns     []Column
	Indexes     []Index
	ForeignKeys []ForeignKey
}

// Shape is a database's tables, and the other objects it holds.
type Shape struct {
	Tables map[string]Table
	// Triggers and Views are refused outright. This build creates neither.
	Triggers, Views []string
}

// ReadShape reads a database's shape. It runs no SQL the database supplies.
func ReadShape(ctx context.Context, db *sql.DB) (*Shape, error) {
	rows, err := db.QueryContext(ctx, `SELECT type, name FROM sqlite_master`)
	if err != nil {
		return nil, err
	}
	var tables []string
	s := &Shape{Tables: map[string]Table{}}
	for rows.Next() {
		var kind, name string
		if err := rows.Scan(&kind, &name); err != nil {
			rows.Close()
			return nil, err
		}
		switch kind {
		case "table":
			tables = append(tables, name)
		case "trigger":
			s.Triggers = append(s.Triggers, name)
		case "view":
			s.Views = append(s.Views, name)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for _, name := range tables {
		t, err := readTable(ctx, db, name)
		if err != nil {
			return nil, fmt.Errorf("table %s: %w", name, err)
		}
		s.Tables[name] = t
	}
	return s, nil
}

func readTable(ctx context.Context, db *sql.DB, name string) (Table, error) {
	var t Table
	rows, err := db.QueryContext(ctx, `SELECT name, type, "notnull", COALESCE(dflt_value, ''), pk, hidden FROM pragma_table_xinfo(?) ORDER BY cid`, name)
	if err != nil {
		return t, err
	}
	for rows.Next() {
		var c Column
		var notNull int
		if err := rows.Scan(&c.Name, &c.Type, &notNull, &c.Default, &c.PrimaryKey, &c.Hidden); err != nil {
			rows.Close()
			return t, err
		}
		c.NotNull, c.Type = notNull != 0, strings.ToUpper(c.Type)
		t.Columns = append(t.Columns, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return t, err
	}

	rows, err = db.QueryContext(ctx, `SELECT name, "unique", origin, partial FROM pragma_index_list(?)`, name)
	if err != nil {
		return t, err
	}
	for rows.Next() {
		var i Index
		var unique, partial int
		if err := rows.Scan(&i.Name, &unique, &i.Origin, &partial); err != nil {
			rows.Close()
			return t, err
		}
		i.Unique, i.Partial = unique != 0, partial != 0
		t.Indexes = append(t.Indexes, i)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return t, err
	}
	for n := range t.Indexes {
		cols, err := db.QueryContext(ctx, `SELECT COALESCE(name, '') FROM pragma_index_info(?) ORDER BY seqno`, t.Indexes[n].Name)
		if err != nil {
			return t, err
		}
		for cols.Next() {
			var c string
			if err := cols.Scan(&c); err != nil {
				cols.Close()
				return t, err
			}
			t.Indexes[n].Columns = append(t.Indexes[n].Columns, c)
		}
		cols.Close()
		if err := cols.Err(); err != nil {
			return t, err
		}
	}

	rows, err = db.QueryContext(ctx, `SELECT "table", "from", COALESCE("to", ''), on_update, on_delete FROM pragma_foreign_key_list(?)`, name)
	if err != nil {
		return t, err
	}
	defer rows.Close()
	for rows.Next() {
		var f ForeignKey
		if err := rows.Scan(&f.Table, &f.From, &f.To, &f.OnUpdate, &f.OnDelete); err != nil {
			return t, err
		}
		t.ForeignKeys = append(t.ForeignKeys, f)
	}
	return t, rows.Err()
}

// CreateSchemaAt makes what this build creates at a migration level, on an empty database, and
// records that level in the migration journal.
func CreateSchemaAt(ctx context.Context, db *sql.DB, level int) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	if level > len(migrations) {
		level = len(migrations)
	}
	if _, err := db.ExecContext(ctx, drizzleTable); err != nil {
		return err
	}
	for _, m := range migrations[:level] {
		for _, stmt := range strings.Split(m.sql, "--> statement-breakpoint") {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("migration %s: %w", m.tag, err)
			}
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO "__drizzle_migrations" (hash, created_at) VALUES (?, ?)`, m.hash, m.createdAt); err != nil {
			return err
		}
	}
	for _, stmt := range goSchema {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// ExpectedShape is what this build creates at a migration level, Go tables included.
func ExpectedShape(ctx context.Context, level int) (*Shape, error) {
	db, err := sql.Open("sqlite", "file:expected-shape?mode=memory&cache=private")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1) // one connection is one in-memory database
	if err := CreateSchemaAt(ctx, db, level); err != nil {
		return nil, err
	}
	return ReadShape(ctx, db)
}

// Unexpected lists what s holds that expected does not: a trigger or view, a table this
// build does not make, a column or index it does not, or one that differs. A table or column
// expected and missing is fine here, because Go tables are added without a migration, so an
// older database lacks some. What it needs and lacks fails when it is opened.
func (s *Shape) Unexpected(expected *Shape) []string {
	var out []string
	for _, name := range s.Triggers {
		out = append(out, "a trigger: "+name)
	}
	for _, name := range s.Views {
		out = append(out, "a view: "+name)
	}
	for name, t := range s.Tables {
		if strings.HasPrefix(name, "sqlite_") {
			continue // SQLite's own, such as sqlite_sequence. A trigger on one is refused above.
		}
		want, ok := expected.Tables[name]
		if !ok {
			out = append(out, "a table this version does not make: "+name)
			continue
		}
		out = append(out, t.unexpected(name, want)...)
	}
	slices.Sort(out)
	return out
}

func (t Table) unexpected(table string, want Table) []string {
	var out []string
	for _, c := range t.Columns {
		i := slices.IndexFunc(want.Columns, func(w Column) bool { return w.Name == c.Name })
		if i < 0 {
			out = append(out, "a column this version does not make: "+table+"."+c.Name)
		} else if want.Columns[i] != c {
			out = append(out, "a column defined differently: "+table+"."+c.Name)
		}
	}
	for _, x := range t.Indexes {
		found := false
		for _, w := range want.Indexes {
			// An automatic index's name depends on its table's history, so only its
			// meaning is compared. One someone created is compared by name too.
			if (x.Origin == "c" && w.Name != x.Name) || w.Origin != x.Origin {
				continue
			}
			if w.Unique == x.Unique && w.Partial == x.Partial && slices.Equal(w.Columns, x.Columns) {
				found = true
				break
			}
		}
		if !found {
			out = append(out, "an index this version does not make: "+table+"."+x.Name)
		}
	}
	for _, f := range t.ForeignKeys {
		if !slices.Contains(want.ForeignKeys, f) {
			out = append(out, "a foreign key this version does not make: "+table+"."+f.From)
		}
	}
	return out
}

// MigrationCount is how many migrations this build knows.
func MigrationCount() (int, error) {
	migrations, err := loadMigrations()
	return len(migrations), err
}

// GoSchemaVersion is the Go schema version this build writes and accepts.
func GoSchemaVersion() int { return goSchemaVersion }
