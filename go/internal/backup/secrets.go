package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// RemovedSecret names something --exclude-secrets took out, and how to put it back. It never
// holds a value.
type RemovedSecret struct {
	Name    string `json:"name"`
	Restore string `json:"restore"`
}

// secretSet is what a root holds that is secret. With --exclude-secrets, reduced holds the
// files that keep what still validates, and dropped the ones left out altogether: a blank
// client secret would stop the gateway, since a refused file stops serve.
type secretSet struct {
	names   []string
	removed []RemovedSecret
	reduced map[string][]byte
	dropped map[string]bool
}

func (s *secretSet) found(name, restore string, exclude bool) {
	if exclude {
		s.removed = append(s.removed, RemovedSecret{Name: name, Restore: restore})
		return
	}
	s.names = append(s.names, name)
}

func readSecrets(ctx context.Context, rootDir string, exclude bool) (*secretSet, error) {
	s := &secretSet{reduced: map[string][]byte{}, dropped: map[string]bool{}}
	hf, otel, err := configSecrets(ctx, filepath.Join(rootDir, "local-base.db"))
	if err != nil {
		return nil, err
	}
	if hf {
		s.found("Hugging Face token", "local-base configure --hf-token <token>", exclude)
	}
	if otel {
		s.found("OTLP headers", "local-base configure --otel-headers <key=value,...>", exclude)
	}
	if err := s.readAccess(rootDir, exclude); err != nil {
		return nil, err
	}
	if err := s.readEmail(rootDir, exclude); err != nil {
		return nil, err
	}
	if err := s.readProfiles(rootDir, exclude); err != nil {
		return nil, err
	}
	sort.Strings(s.names)
	return s, nil
}

// readJSON reads a root's file through the check that refuses a link or another user's file.
func readJSON(path string) ([]byte, map[string]any, error) {
	raw, err := obs.ReadOwnedRegular(path)
	if err != nil {
		return nil, nil, err
	}
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, nil, fmt.Errorf("%s is not valid JSON", filepath.Base(path))
	}
	return raw, doc, nil
}

func (s *secretSet) readAccess(rootDir string, exclude bool) error {
	const rel = "ui-access.json"
	_, doc, err := readJSON(filepath.Join(rootDir, rel))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	provider, _ := doc["provider"].(map[string]any)
	regs, _ := provider["registrations"].([]any)
	var kept []any
	for _, item := range regs {
		reg, _ := item.(map[string]any)
		kind, _ := reg["kind"].(string)
		id, _ := reg["id"].(string)
		name, _ := reg["name"].(string)
		secret, _ := reg["clientSecret"].(string)
		if auth, ok := reg["clientAuthentication"].(map[string]any); ok && secret == "" {
			secret, _ = auth["clientSecret"].(string)
		}
		if secret == "" {
			kept = append(kept, item)
			continue
		}
		switch kind {
		case "oidc":
			issuer, _ := reg["issuer"].(string)
			client, _ := reg["clientId"].(string)
			s.found("OIDC client secret ("+id+")", fmt.Sprintf("local-base access oidc add --id %s --name %q --issuer %s --client-id %s --client-secret-env <ENVIRONMENT_VARIABLE>", id, name, issuer, client), exclude)
		default:
			s.found("GitHub client secret ("+id+")", "local-base access github-oauth add --id "+id, exclude)
		}
	}
	if !exclude || len(kept) == len(regs) {
		return nil
	}
	if len(kept) == 0 {
		s.dropped[rel] = true
		return nil
	}
	provider["registrations"] = kept
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	s.reduced[rel] = append(out, '\n')
	return nil
}

func (s *secretSet) readEmail(rootDir string, exclude bool) error {
	const rel = "email-delivery.json"
	_, doc, err := readJSON(filepath.Join(rootDir, rel))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	auth, _ := doc["authentication"].(map[string]any)
	if password, _ := auth["password"].(string); password == "" {
		return nil
	}
	s.found("SMTP password", "local-base access email configure", exclude)
	if exclude {
		s.dropped[rel] = true
	}
	return nil
}

func (s *secretSet) readProfiles(rootDir string, exclude bool) error {
	rel := "credentials/auth-profiles.json"
	_, doc, err := readJSON(filepath.Join(rootDir, filepath.FromSlash(rel)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	profiles, _ := doc["profiles"].([]any)
	for _, item := range profiles {
		p, _ := item.(map[string]any)
		id, _ := p["id"].(string)
		var origins, headers []string
		if list, ok := p["origins"].([]any); ok {
			for _, o := range list {
				if v, ok := o.(string); ok {
					origins = append(origins, "--origin "+v)
				}
			}
		}
		if h, ok := p["headers"].(map[string]any); ok {
			for name := range h {
				headers = append(headers, "--header-env "+name+"=<ENVIRONMENT_VARIABLE>")
			}
		}
		sort.Strings(headers)
		s.found("download credential ("+id+")", strings.TrimSpace("local-base auth-profiles add --id "+id+" "+strings.Join(append(origins, headers...), " ")), exclude)
	}
	// A profile holds nothing but its secret values, so the file goes whole.
	if exclude {
		s.dropped[rel] = true
	}
	return nil
}

// sqliteDSN makes a path safe in a SQLite URI, for a place someone chose with --output.
func sqliteDSN(path string, query string) string {
	return (&url.URL{Scheme: "file", Path: path, RawQuery: query}).String()
}

func hasColumn(ctx context.Context, db *sql.DB, table, column string) bool {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n)
	return err == nil && n > 0
}

func hasTable(ctx context.Context, db *sql.DB, table string) bool {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n)
	return err == nil && n > 0
}

// readableDatabase says whether a database can be read without changing anything.
func readableDatabase(ctx context.Context, path string) error {
	db, err := openReadOnly(path)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }() // Read-only.
	var n int
	return db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master`).Scan(&n)
}

// isUnfinishedWrite is a database whose journal was left by a process that stopped mid-write. Its
// file can't be trusted until SQLite plays that journal back, which takes opening it for writing.
// A process still writing holds a lock, so its journal doesn't count.
func isUnfinishedWrite(err error) bool {
	var e *sqlite.Error
	return errors.As(err, &e) && e.Code() == sqlite3.SQLITE_READONLY_ROLLBACK
}

func unfinishedWriteMessage(rootDir string) string {
	return fmt.Sprintf("The database on %s has a write that never finished, left by a process that stopped. Nothing was changed. To undo that write, run: local-base --root '%s' status. Then try again.", rootDir, rootDir)
}

func openReadOnly(path string) (*sql.DB, error) {
	return sql.Open("sqlite", sqliteDSN(path, "mode=ro&_pragma=busy_timeout(5000)&_pragma=trusted_schema(OFF)"))
}

// configSecrets reads the database directly: an older one may lack a column, and being wrong
// here would print the wrong warning.
func configSecrets(ctx context.Context, dbPath string) (hf, otel bool, err error) {
	db, err := openReadOnly(dbPath)
	if err != nil {
		return false, false, err
	}
	defer db.Close()
	read := func(column string) (bool, error) {
		if !hasColumn(ctx, db, "config", column) {
			return false, nil
		}
		var n int
		err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM config WHERE `+column+` != ''`).Scan(&n)
		return n > 0, err
	}
	if hf, err = read("hf_token"); err != nil {
		return false, false, err
	}
	otel, err = read("otel_headers")
	return hf, otel, err
}

func appliedMigrations(ctx context.Context, dbPath string) (int, error) {
	db, err := openReadOnly(dbPath)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	return migrationLevel(ctx, db)
}

func migrationLevel(ctx context.Context, db *sql.DB) (int, error) {
	if !hasTable(ctx, db, "__drizzle_migrations") {
		return 0, nil
	}
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM "__drizzle_migrations"`).Scan(&n)
	return n, err
}

func presentRows(ctx context.Context, dbPath string) []string {
	db, err := openReadOnly(dbPath)
	if err != nil {
		return nil
	}
	defer db.Close()
	var out []string
	for _, t := range rowsLeftOut {
		if hasTable(ctx, db, t) {
			out = append(out, t)
		}
	}
	return out
}

// selectedModels are the ids a restored root would want installed. It reads through the store,
// and an older database that can't be read that way simply lists none.
func selectedModels(ctx context.Context, dbPath string) []string {
	st, err := store.OpenReadOnly(dbPath)
	if err != nil {
		return nil
	}
	defer func() { _ = st.Close() }() // Read-only.
	cfg, err := st.LoadConfig(ctx)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	out := []string{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, ids := range cfg.Selected {
		for _, id := range ids {
			add(id)
		}
	}
	for _, id := range cfg.Active {
		add(id)
	}
	sort.Strings(out)
	return out
}

// dropRows removes what a backup and a restored root must never hold.
func dropRows(ctx context.Context, db *sql.DB) error {
	for _, t := range rowsLeftOut {
		if !hasTable(ctx, db, t) {
			continue
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM `+t); err != nil { //nolint:gosec // G202: t is from the constant list rowsLeftOut, and a placeholder can't name a table.
			return fmt.Errorf("%s: %w", t, err)
		}
	}
	if hasTable(ctx, db, "go_settings") {
		// So that start shows a first-run link again, on a root everyone is signed out of.
		if _, err := db.ExecContext(ctx, `DELETE FROM go_settings WHERE key = 'access.first-run-link-shown'`); err != nil {
			return err
		}
	}
	return nil
}

// copyDatabase makes the database's copy for a backup. VACUUM INTO is a consistent copy that
// blocks no reader, and a second one leaves out the pages of what was just removed, so a removed
// secret is not left in free pages.
func copyDatabase(ctx context.Context, from, to, stage string, exclude bool) error {
	source, err := openReadOnly(from)
	if err != nil {
		return err
	}
	// VACUUM INTO takes an empty file, and keeps its mode.
	first := filepath.Join(stage, "database-first")
	if err := writePrivate(first, nil); err != nil {
		source.Close()
		return err
	}
	_, err = source.ExecContext(ctx, `VACUUM INTO ?`, first)
	source.Close()
	if err != nil {
		return fmt.Errorf("copying the database: %w", err)
	}
	defer os.Remove(first)
	db, err := sql.Open("sqlite", sqliteDSN(first, "_pragma=trusted_schema(OFF)"))
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := dropRows(ctx, db); err != nil {
		return err
	}
	if hasColumn(ctx, db, "config", "root") {
		// A backup names no path. Restore sets this to the root it restores into.
		if _, err := db.ExecContext(ctx, `UPDATE config SET root = ''`); err != nil {
			return err
		}
	}
	if exclude {
		for _, column := range []string{"hf_token", "otel_headers"} {
			if hasColumn(ctx, db, "config", column) {
				if _, err := db.ExecContext(ctx, `UPDATE config SET `+column+` = ''`); err != nil { //nolint:gosec // G202: column is from the constant list above, and a placeholder can't name a column.
					return err
				}
			}
		}
	}
	if err := writePrivate(to, nil); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, to); err != nil {
		return fmt.Errorf("copying the database: %w", err)
	}
	return nil
}

func ownedByMe(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
