package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The migrations are the 0.1.0 set, byte for byte. A rollback to 0.1.0 is a binary swap only
// because its drizzle migrator finds the same rows in __drizzle_migrations: each row's hash is the
// SQL file's sha256, and its created_at is the journal's "when". So no file here may change, and
// no Go-only migration may be added, until a release explicitly drops the 0.1.0 rollback and the
// CHANGELOG says so (docs/typescript-retirement.md). That release updates this list.
var v010Migrations = map[string]string{
	"0000_useful_maggott.sql":             "1b9cd0e857fa7e10d47f6e8d53ef1913f50f17d95f06920add403be9190d5479",
	"0001_video_runtime.sql":              "5d6ae6326c57ecd5afadc548dc2bcab8c7a78b31749ef0444a664be4a15ccb43",
	"0002_api_key_scopes.sql":             "0bdbc766565e9f4fcf1fd8d4422d8599461af98ea030712bc7cdb1616169bc0a",
	"0003_declarative_configuration.sql":  "aa59c8516244e4e8fed7e891da5ec8fa0046a4e48dfcca0c9a921b39e6ecb06a",
	"0004_sturdy_dark_phoenix.sql":        "2d0d38623aedf936e41085e62d2ae88d01dea74c13915e2e6485e0133907aeb2",
	"0005_bent_spencer_smythe.sql":        "e3dc135ab67f43b673af58cdc81dc351e5d6dbad2c7444028cafaa9bf781a407",
	"0006_curvy_vivisector.sql":           "c0e22049ba657b7c196fd71b18b871fe45c238dacca512d367cda201e2a15223",
	"0007_romantic_puff_adder.sql":        "33d251437d55f4005f795ffa5aedf2b264b97551dee6675a22fa11d41ed5098a",
	"0008_majestic_spirit.sql":            "0de823902472120a045d636287b3f62e10d74d1ff85ee261c0eccbab15ae219e",
	"0009_third_whizzer.sql":              "823255d7df01829e8fb12a0715cce7b14d6c2ccc7c4a88327c010962b8bbea84",
	"0010_wooden_gamma_corps.sql":         "e928d750e8f45be6d867f4edbe59ae112325b523322e47473aadce731fba9fe4",
	"0011_same_miss_america.sql":          "136459c7b1738b137ae45d3bd400ee335c086d59919c786f7a4c08924cb6cfd0",
	"0012_model_runtime_capabilities.sql": "7ab5da88c6ba40abcde7dd1c4effaabacf814db102e10400c16b81da546c6279",
	"0013_model_observed_memory.sql":      "ecd20c9f664c5eadc66f13e619a215e44b0b533b7f12a1b97a27515129e089dd",
	"0014_tts_custom_voices.sql":          "8a10774c6adfe82e38034da3b5c8bd39ca2c2601bc2de9ec7041707f908878ef",
	"meta/_journal.json":                  "3f823b76105ca91fcadf466e5b7730f0962b0b8833fb8aa65386ffab56ff57f1",
}

// What the binary embeds is exactly the pinned set: no file changed, none added, none missing.
func TestMigrationsAreTheV010Set(t *testing.T) {
	seen := map[string]bool{}
	err := fs.WalkDir(migrationFS, "migrations", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		name := strings.TrimPrefix(path, "migrations/")
		seen[name] = true
		body, err := migrationFS.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		want, ok := v010Migrations[name]
		switch {
		case !ok:
			t.Errorf("%s is not a 0.1.0 migration: no new migration until a release drops the 0.1.0 rollback (see the comment above)", name)
		case hex.EncodeToString(sum[:]) != want:
			t.Errorf("%s changed: its sha256 is in every 0.1.0 database's __drizzle_migrations, so its bytes must not change", name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for name := range v010Migrations {
		if !seen[name] {
			t.Errorf("%s is missing from the embedded migrations", name)
		}
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
