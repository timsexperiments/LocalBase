package backup

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// craftDB edits a copy of a backup's database and packs it as another file.
func craftDB(t *testing.T, good string, statements ...string) string {
	t.Helper()
	e := read(t, good)
	editDB(t, e, statements...)
	crafted := filepath.Join(dir(t), "crafted"+Extension)
	pack(t, crafted, e)
	return crafted
}

// What SQLite's pragmas don't show is in the definition it keeps: a CHECK, a COLLATE, an index's order or
// condition. A table or index LocalBase makes has the same definition wherever it was made, so a different
// one was changed by something else, and a missing index is refused like a missing column.
func TestRestoreRefusesADefinitionItDoesNotMake(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	good := create(t, r, false).Output
	for name, c := range map[string]struct {
		statements []string
		says       []string
	}{
		// Security's case: every revocation afterwards fails.
		"a CHECK that stops revoking": {[]string{"UPDATE api_keys SET revoked_at = NULL", "PRAGMA writable_schema=ON",
			"UPDATE sqlite_master SET sql = replace(sql, '`revoked_at` text', '`revoked_at` text CHECK (revoked_at IS NULL)') WHERE name = 'api_keys'"},
			[]string{"a table defined differently: api_keys"}},
		"a COLLATE on a column": {[]string{"PRAGMA writable_schema=ON",
			"UPDATE sqlite_master SET sql = replace(sql, '`name` text NOT NULL', '`name` text NOT NULL COLLATE NOCASE') WHERE name = 'api_keys'"},
			[]string{"a table defined differently: api_keys"}},
		"an index in another order": {[]string{"DROP INDEX auth_roles_name_unique", "CREATE UNIQUE INDEX auth_roles_name_unique ON auth_roles (name DESC)"},
			[]string{"an index defined differently: auth_roles_name_unique"}},
		"an index with a condition": {[]string{"DROP INDEX auth_roles_name_unique", "CREATE UNIQUE INDEX auth_roles_name_unique ON auth_roles (name) WHERE name != ''"},
			[]string{"an index defined differently: auth_roles_name_unique"}},
		"a missing index": {[]string{"DROP INDEX auth_roles_name_unique"},
			[]string{"an index this version needs is missing: auth_roles_name_unique"}},
		// SQLite reads a non-breaking space as part of a word, so this column's type swallows NOT, and NOT NULL is
		// gone, while a text compare that collapses every kind of space would see the same definition.
		"a non-breaking space": {[]string{"PRAGMA writable_schema=ON",
			"UPDATE sqlite_master SET sql = replace(sql, '`name` text NOT NULL', '`name` text' || char(160) || 'NOT NULL') WHERE name = 'api_keys'"},
			[]string{"a table defined differently: api_keys"}},
		"a missing foreign key": {[]string{"PRAGMA writable_schema=ON",
			"UPDATE sqlite_master SET sql = replace(sql, 'FOREIGN KEY (`role_id`) REFERENCES `auth_roles`(`id`) ON UPDATE no action ON DELETE cascade', 'CHECK (1)') WHERE name = 'auth_role_permissions'"},
			[]string{"a foreign key this version needs is missing: auth_role_permissions.role_id", "a table defined differently: auth_role_permissions"}},
	} {
		t.Run(name, func(t *testing.T) {
			parent := dir(t)
			_, err := Stage(context.Background(), craftDB(t, good, c.statements...), filepath.Join(parent, "target"), false)
			if err == nil || !strings.HasPrefix(err.Error(), "The database holds what LocalBase does not make, so it was changed by something else: ") {
				t.Fatalf("restore: %v", err)
			}
			for _, want := range c.says {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("it should say %q: %v", want, err)
				}
			}
			noLeftovers(t, parent)
		})
	}
}

// Whitespace in a definition isn't a difference: SQLite keeps a statement as it was written.
func TestADefinitionDifferingOnlyInWhitespaceIsRestored(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	good := create(t, r, false).Output
	crafted := craftDB(t, good, "PRAGMA writable_schema=ON",
		"UPDATE sqlite_master SET sql = replace(replace(sql, '`id` text PRIMARY KEY', '`id`   text\n\tPRIMARY KEY'), '`name` text', '`name`  text') WHERE name = 'api_keys'")
	restore(t, crafted, filepath.Join(dir(t), "target"), false)
}

// Go's own tables are added on open, so a backup from before one lacks it, and that's fine. One that's there
// is compared like the rest: they hold sign-in state and settings, and a CHECK on go_settings would make
// turning an import opt-in off fail while it stays on.
func TestAGoTableDefinedDifferentlyIsRefused(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	good := create(t, r, false).Output
	parent := dir(t)
	crafted := craftDB(t, good, "PRAGMA writable_schema=ON",
		"UPDATE sqlite_master SET sql = rtrim(rtrim(sql), ')') || ', CHECK (value != ''off''))' WHERE name = 'go_settings'")
	_, err := Stage(context.Background(), crafted, filepath.Join(parent, "target"), false)
	if err == nil || !strings.Contains(err.Error(), "a table defined differently: go_settings") {
		t.Fatalf("restore: %v", err)
	}
	noLeftovers(t, parent)
}

// The migration journal's table is made by TypeScript's migrator too, and isn't compared: a root it made can
// have written it differently.
func TestTheMigrationJournalIsLeftAsItWasMade(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	good := create(t, r, false).Output
	crafted := craftDB(t, good, "PRAGMA writable_schema=ON",
		"UPDATE sqlite_master SET sql = replace(sql, 'SERIAL PRIMARY KEY', 'SERIAL PRIMARY KEY /* as another migrator wrote it */') WHERE name = '__drizzle_migrations'")
	restore(t, crafted, filepath.Join(dir(t), "target"), false)
}
