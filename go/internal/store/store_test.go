package store

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The embedded copy must match the TS drizzle folder until TS retires.
func TestEmbeddedMigrationsMatchDrizzle(t *testing.T) {
	src := filepath.Join("..", "..", "..", "drizzle")
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		embedded, _ := migrationFS.ReadFile("migrations/" + e.Name())
		original, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatalf("%s missing from drizzle/: %v", e.Name(), err)
		}
		if !bytes.Equal(embedded, original) {
			t.Errorf("%s differs from drizzle/; re-copy it", e.Name())
		}
	}
	j1, _ := migrationFS.ReadFile("migrations/meta/_journal.json")
	j2, _ := os.ReadFile(filepath.Join(src, "meta", "_journal.json"))
	if !bytes.Equal(j1, j2) {
		t.Error("_journal.json differs from drizzle/meta")
	}
}

func TestFreshDatabaseJournalIsDrizzleCompatible(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-base.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	migrations, _ := loadMigrations()
	rows, err := s.DB.Query(`SELECT hash, created_at FROM "__drizzle_migrations" ORDER BY created_at`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var hash string
		var created int64
		rows.Scan(&hash, &created)
		if hash != migrations[i].hash || created != migrations[i].createdAt {
			t.Fatalf("row %d = %s/%d, want %s/%d", i, hash, created, migrations[i].hash, migrations[i].createdAt)
		}
		i++
	}
	if i != len(migrations) {
		t.Fatalf("applied %d migrations, want %d", i, len(migrations))
	}
	// Reopening is a no-op.
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()
}

// LOCALBASE_TEST_TS_DB points at a database created by the TS binary (a
// copy, never the live file). Go must open it without migrating and read it.
func TestOpensTypeScriptDatabase(t *testing.T) {
	src := os.Getenv("LOCALBASE_TEST_TS_DB")
	if src == "" {
		t.Skip("set LOCALBASE_TEST_TS_DB to a copy of a TS-created local-base.db")
	}
	dst := filepath.Join(t.TempDir(), "local-base.db")
	if out, err := exec.Command("sqlite3", src, ".backup "+dst).CombinedOutput(); err != nil {
		t.Fatalf("backup: %v %s", err, out)
	}
	before := journalCount(t, dst)
	s, err := Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if after := journalCount(t, dst); after != before {
		t.Fatalf("journal rows changed %s -> %s", before, after)
	}
	cfg, err := s.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Active["llm"] == "" {
		t.Fatal("no active llm")
	}
	if _, err := s.ListAPIKeys(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func journalCount(t *testing.T, path string) string {
	out, err := exec.Command("sqlite3", path, `select count(*) || ':' || max(created_at) from __drizzle_migrations`).Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
