package store

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
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

// Definition is a table's or an index's statement as SQLite keeps it, with whitespace collapsed.
type Definition struct {
	Type, Name, Table, SQL string
}

// Shape is a database's tables, and the other objects it holds.
type Shape struct {
	Tables map[string]Table
	// Definitions are by type and name. They hold what no pragma says: a CHECK, a COLLATE, an index's
	// order or condition.
	Definitions map[string]Definition
	// Triggers and Views are refused outright. This build creates neither.
	Triggers, Views []string
}

// ReadShape reads a database's shape. It runs no SQL the database supplies.
func ReadShape(ctx context.Context, db *sql.DB) (*Shape, error) {
	rows, err := db.QueryContext(ctx, `SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master`)
	if err != nil {
		return nil, err
	}
	var tables []string
	s := &Shape{Tables: map[string]Table{}, Definitions: map[string]Definition{}}
	for rows.Next() {
		var kind, name, table, text string
		if err := rows.Scan(&kind, &name, &table, &text); err != nil {
			rows.Close()
			return nil, err
		}
		// An index SQLite makes for a constraint has no statement of its own: its table's says it.
		if (kind == "table" || kind == "index") && text != "" {
			s.Definitions[kind+" "+name] = Definition{Type: kind, Name: name, Table: table, SQL: collapse(text)}
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

var goTableRe = regexp.MustCompile(`CREATE TABLE IF NOT EXISTS ([a-z_]+)`)

// goTable says whether this build adds a table on open, rather than through a migration. Those
// are added without raising a version (goSchemaVersion), so an older database at the same level
// can lack them, and opening it creates them.
func goTable(name string) bool {
	if name == "__drizzle_migrations" {
		return true
	}
	for _, stmt := range goSchema {
		if m := goTableRe.FindStringSubmatch(stmt); m != nil && m[1] == name {
			return true
		}
	}
	return false
}

// GoTables lists the tables goTable covers, for tests.
func GoTables() []string {
	var out []string
	for _, stmt := range goSchema {
		if m := goTableRe.FindStringSubmatch(stmt); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

// Unexpected lists how s differs from expected: a trigger or view, a table this build does not
// make, a column or index it does not, or one that differs. A Go table may be missing, since
// opening the database creates it. A table or column a migration makes may not.
func (s *Shape) Unexpected(expected *Shape) []string {
	var out []string
	for name, want := range expected.Tables {
		t, here := s.Tables[name]
		if !here {
			if !goTable(name) {
				out = append(out, "a table this version needs is missing: "+name)
			}
			continue
		}
		if goTable(name) {
			continue
		}
		for _, c := range want.Columns {
			if !slices.ContainsFunc(t.Columns, func(have Column) bool { return have.Name == c.Name }) {
				out = append(out, "a column this version needs is missing: "+name+"."+c.Name)
			}
		}
	}
	for _, name := range s.Triggers {
		out = append(out, "a trigger: "+name)
	}
	for _, name := range s.Views {
		out = append(out, "a view: "+name)
	}
	// A table or index is made by the same statements wherever it was made, so its definition is the same,
	// apart from whitespace. Go's own tables have only ever been added to, never changed: a backup from before
	// one can lack it, as opening the database makes it, but one that's there is compared like the rest.
	for key, want := range expected.Definitions {
		// TypeScript's migrator made this one, and other versions of it may have written it differently.
		if want.Table == drizzleTableName {
			continue
		}
		have, here := s.Definitions[key]
		_, tableHere := s.Tables[want.Table]
		switch {
		case !here && want.Type == "index" && tableHere:
			out = append(out, "an index this version needs is missing: "+want.Name)
		case !here || have.SQL == want.SQL:
			// A missing table is said above, and a missing Go table is allowed.
		case want.Type == "table":
			out = append(out, "a table defined differently: "+want.Name)
		default:
			out = append(out, "an index defined differently: "+want.Name)
		}
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
	// Missing counts too: a table rebuilt without its foreign key can hold rows nothing would catch.
	for _, f := range want.ForeignKeys {
		if !slices.Contains(t.ForeignKeys, f) {
			out = append(out, "a foreign key this version needs is missing: "+table+"."+f.From)
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

// collapse runs together only the whitespace SQLite's tokenizer skips. SQLite reads other spaces, such as a
// non-breaking one, as part of a word, so collapsing those too could make two different definitions look the
// same.
func collapse(sql string) string {
	return strings.Join(strings.FieldsFunc(sql, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\f' || r == '\r'
	}), " ")
}

// drizzleTableName is the migration journal's table, which the TypeScript build makes too.
const drizzleTableName = "__drizzle_migrations"
