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
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// RemovedSecret names something --exclude-secrets took out, and how to put it back. It never
// holds a value. A restore builds its command from the fields, checked, and never prints Restore,
// which a file can say anything in. Restore stays for older versions, which print it.
type RemovedSecret struct {
	Name    string `json:"name"`
	Restore string `json:"restore"`
	Kind    string `json:"kind,omitempty"`
	ID      string `json:"id,omitempty"`
	// Label is a sign-in provider's name, for --name.
	Label    string   `json:"label,omitempty"`
	Issuer   string   `json:"issuer,omitempty"`
	ClientID string   `json:"clientId,omitempty"`
	Origins  []string `json:"origins,omitempty"`
	// Headers are names only.
	Headers []string `json:"headers,omitempty"`
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

func (s *secretSet) found(r RemovedSecret, exclude bool) {
	r.Name = secretName(r)
	if !exclude {
		s.names = append(s.names, r.Name)
		return
	}
	// This root's own data, so the command is the same whether or not a restore could check it.
	r.Restore, _ = putBackCommand(r, false)
	s.removed = append(s.removed, r)
}

func readSecrets(ctx context.Context, rootDir, dbPath string, exclude bool) (*secretSet, error) {
	s := &secretSet{reduced: map[string][]byte{}, dropped: map[string]bool{}}
	hf, otel, err := configSecrets(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	if hf {
		s.found(RemovedSecret{Kind: kindHFToken}, exclude)
	}
	if otel {
		s.found(RemovedSecret{Kind: kindOTLPHeaders}, exclude)
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
			s.found(RemovedSecret{Kind: kindOIDC, ID: id, Label: name, Issuer: issuer, ClientID: client}, exclude)
		default:
			s.found(RemovedSecret{Kind: kindGitHub, ID: id}, exclude)
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
	s.found(RemovedSecret{Kind: kindSMTP}, exclude)
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
					origins = append(origins, v)
				}
			}
		}
		if h, ok := p["headers"].(map[string]any); ok {
			for name := range h {
				headers = append(headers, name)
			}
		}
		sort.Strings(headers)
		s.found(RemovedSecret{Kind: kindDownload, ID: id, Origins: origins, Headers: headers}, exclude)
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

// UnfinishedWrite says what to do about a root whose database a process left mid-write, or ""
// when there is none. It changes nothing.
func UnfinishedWrite(ctx context.Context, rootDir string) string {
	dbPath := filepath.Join(rootDir, "local-base.db")
	// In write-ahead logging SQLite recovers a crash by itself, and reading this way would leave files.
	if store.InWAL(dbPath) {
		return ""
	}
	if err := readableDatabase(ctx, dbPath); isUnfinishedWrite(err) {
		return unfinishedWriteMessage(rootDir)
	}
	return ""
}

func unfinishedWriteMessage(rootDir string) string {
	return fmt.Sprintf("The database on %s has a write that never finished, left by a process that stopped. Nothing was changed. To undo that write, run: local-base --root '%s' status. Then try again.", rootDir, rootDir)
}

// readRoot says which file to read a root's database through, for all of one command's reads. Reading
// it directly would leave a -shm and a -wal behind on a stopped root, and fail on one that can't be
// written. done removes any private copy. A read that fails for a reason of its own comes back here as
// no error, so that the step that needs the data can say what happened.
func readRoot(ctx context.Context, dbPath string) (read string, done func(), err error) {
	read, done, err = store.ReadablePath(dbPath)
	if err != nil || read != dbPath {
		return read, done, err
	}
	err = readableDatabase(ctx, read)
	if err == nil || isBusy(err) || isUnfinishedWrite(err) {
		return read, done, nil
	}
	// It's read in place because a -shm is there. That file may be what's wrong, and a copy doesn't use it.
	copied, removeCopy, err := store.PrivateCopy(dbPath)
	if err != nil {
		return read, done, nil
	}
	return copied, removeCopy, nil
}

func openReadOnly(path string) (*sql.DB, error) {
	return sql.Open("sqlite", sqliteDSN(path, fmt.Sprintf("mode=ro&_pragma=busy_timeout(%d)&_pragma=trusted_schema(OFF)", readBusyMillis)))
}

// A backup runs beside a live gateway, and a rollback journal lets a writer keep readers out while
// it commits. SQLite's own timeout isn't fair to a reader, so a busy spell can outlast it.
var (
	// readBusyMillis is how long one read waits on SQLite. busyWait is how long a step keeps trying.
	// Variables so that a test needn't take a minute.
	readBusyMillis = 5000
	busyWait       = 60 * time.Second
)

const busyRetryPause = 100 * time.Millisecond

func isBusy(err error) bool {
	var e *sqlite.Error
	if !errors.As(err, &e) {
		return false
	}
	code := e.Code() & 0xff
	return code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED
}

// retryBusy runs a read until the database lets it, for up to busyWait. The read must be safe to
// run again.
func retryBusy(ctx context.Context, read func() error) error {
	deadline := time.Now().Add(busyWait)
	for {
		err := read()
		if !isBusy(err) || !time.Now().Before(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(busyRetryPause):
		}
	}
}

// quote makes a path safe to paste into a shell.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// busyMessage is what a person reads when the database stayed busy, with the command to run again.
// It names no command to stop anything: `local-base stop` also disables the service, which is far
// too much for a lock that will pass.
// Its last words name the step, so that a report says where, and no raw SQLite error reaches them.
func busyMessage(rootDir, retry string, err error) string {
	step, _, _ := strings.Cut(err.Error(), ":")
	return fmt.Sprintf("The database on %s is busy: the gateway or another command is writing to it. Nothing was changed. Try again in a moment: %s. If it keeps happening, stop the gateway, or whatever else is writing. (This was while %s.)", rootDir, retry, step)
}

// tableExists and columnExists say so when they can't tell. An answer of "no" on a failed read once
// made a busy database look like one with no migrations, no models and no revocations.
func tableExists(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n)
	return n > 0, err
}

// tablesExist says whether a database has all of these. A root last opened by an older version lacks
// the tables added since, which is having no rows in them, not a failed read.
func tablesExist(ctx context.Context, db *sql.DB, tables ...string) (bool, error) {
	for _, table := range tables {
		if present, err := tableExists(ctx, db, table); err != nil || !present {
			return false, err
		}
	}
	return true, nil
}

func columnExists(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n)
	return n > 0, err
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
		present, err := columnExists(ctx, db, "config", column)
		if err != nil || !present {
			return false, err
		}
		var n int
		err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM config WHERE `+column+` != ''`).Scan(&n) //nolint:gosec // G202: column is one of the two names below, and a placeholder can't name a column.
		return n > 0, err
	}
	if hf, err = read("hf_token"); err != nil {
		return false, false, fmt.Errorf("reading the settings: %w", err)
	}
	if otel, err = read("otel_headers"); err != nil {
		return false, false, fmt.Errorf("reading the settings: %w", err)
	}
	return hf, otel, nil
}

func appliedMigrations(ctx context.Context, dbPath string) (int, error) {
	db, err := openReadOnly(dbPath)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	level, err := migrationLevel(ctx, db)
	if err != nil {
		return 0, fmt.Errorf("reading the migrations: %w", err)
	}
	return level, nil
}

func migrationLevel(ctx context.Context, db *sql.DB) (int, error) {
	present, err := tableExists(ctx, db, "__drizzle_migrations")
	if err != nil || !present {
		return 0, err
	}
	var n int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM "__drizzle_migrations"`).Scan(&n)
	return n, err
}

func presentRows(ctx context.Context, dbPath string) ([]string, error) {
	db, err := openReadOnly(dbPath)
	if err != nil {
		return nil, fmt.Errorf("reading the tables: %w", err)
	}
	defer db.Close()
	var out []string
	for _, t := range rowsLeftOut {
		present, err := tableExists(ctx, db, t)
		if err != nil {
			return nil, fmt.Errorf("reading the tables: %w", err)
		}
		if present {
			out = append(out, t)
		}
	}
	return out, nil
}

// selectedModels are the ids a restored root would want installed. It reads through the store. An
// older database that can't be read that way lists none, and any other failure is a failure.
func selectedModels(ctx context.Context, dbPath string) ([]string, error) {
	unavailable := func(err error) ([]string, error) {
		if errors.Is(err, store.ErrNotConfigured) || strings.Contains(err.Error(), "no such ") {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the models: %w", err)
	}
	db, err := openReadOnly(dbPath)
	if err != nil {
		return unavailable(err)
	}
	defer func() { _ = db.Close() }() // Read-only.
	cfg, err := (&store.Store{DB: db}).LoadConfig(ctx)
	if err != nil {
		return unavailable(err)
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
	return out, nil
}

// dropRows removes what a backup and a restored root must never hold.
func dropRows(ctx context.Context, db *sql.DB) error {
	for _, t := range rowsLeftOut {
		present, err := tableExists(ctx, db, t)
		if err != nil {
			return fmt.Errorf("%s: %w", t, err)
		}
		if !present {
			continue
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM `+t); err != nil { //nolint:gosec // G202: t is from the constant list rowsLeftOut, and a placeholder can't name a table.
			return fmt.Errorf("%s: %w", t, err)
		}
	}
	settings, err := tableExists(ctx, db, "go_settings")
	if err != nil {
		return err
	}
	if settings {
		// So that start shows a first-run link again, on a root everyone is signed out of.
		if _, err := db.ExecContext(ctx, `DELETE FROM go_settings WHERE key = 'access.first-run-link-shown'`); err != nil {
			return err
		}
		// What the last check for a newer version found: a file's own answer would show as available.
		for _, key := range updateCache {
			if _, err := db.ExecContext(ctx, `DELETE FROM go_settings WHERE key = ?`, key); err != nil {
				return err
			}
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
	err = retryBusy(ctx, func() error {
		if err := os.Remove(first); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := writePrivate(first, nil); err != nil {
			return err
		}
		_, err := source.ExecContext(ctx, `VACUUM INTO ?`, first)
		return err
	})
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
		return fmt.Errorf("cleaning the copy: %w", err)
	}
	hasRoot, err := columnExists(ctx, db, "config", "root")
	if err != nil {
		return fmt.Errorf("cleaning the copy: %w", err)
	}
	if hasRoot {
		// A backup names no path. Restore sets this to the root it restores into.
		if _, err := db.ExecContext(ctx, `UPDATE config SET root = ''`); err != nil {
			return fmt.Errorf("cleaning the copy: %w", err)
		}
	}
	if exclude {
		for _, column := range []string{"hf_token", "otel_headers"} {
			present, err := columnExists(ctx, db, "config", column)
			if err != nil {
				return fmt.Errorf("cleaning the copy: %w", err)
			}
			if present {
				if _, err := db.ExecContext(ctx, `UPDATE config SET `+column+` = ''`); err != nil { //nolint:gosec // G202: column is from the constant list above, and a placeholder can't name a column.
					return fmt.Errorf("cleaning the copy: %w", err)
				}
			}
		}
	}
	if err := writePrivate(to, nil); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, to); err != nil {
		return fmt.Errorf("writing the copy: %w", err)
	}
	return nil
}

func ownedByMe(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
