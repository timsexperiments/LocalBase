package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The Go extension schema holds state the TS implementation does not have.
// It is not a drizzle migration: TS rejects a database whose
// __drizzle_migrations rows differ from its journal, which would break
// binary-swap rollback. These tables are created idempotently and recorded
// in go_schema; TS ignores tables it does not know.

const goSchemaVersion = 1

var goSchema = []string{
	`CREATE TABLE IF NOT EXISTS go_schema (id INTEGER PRIMARY KEY CHECK (id = 1), version INTEGER NOT NULL)`,
	// Imported logical models (models.Model JSON). Device-local choices are
	// never stored here.
	`CREATE TABLE IF NOT EXISTS imported_models (
		id TEXT PRIMARY KEY,
		definition TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	// Manual backend choices, per model and device. Automatic mode is the
	// absence of a row.
	`CREATE TABLE IF NOT EXISTS model_backend_selections (
		model_id TEXT NOT NULL,
		device_id TEXT NOT NULL,
		backend_id TEXT NOT NULL,
		variant_id TEXT,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (model_id, device_id)
	)`,
}

func applyGoSchema(ctx context.Context, tx *sql.Tx) error {
	for _, stmt := range goSchema {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("go schema: %w", err)
		}
	}
	var v int
	err := tx.QueryRowContext(ctx, `SELECT version FROM go_schema WHERE id = 1`).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx, `INSERT INTO go_schema (id, version) VALUES (1, ?)`, goSchemaVersion)
		return err
	case err != nil:
		return err
	case v > goSchemaVersion:
		return fmt.Errorf("database extension schema %d is newer than this build (%d)", v, goSchemaVersion)
	}
	return nil
}

func nowISO() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

// ImportedModel is a stored model definition (JSON).
type ImportedModel struct {
	ID         string
	Definition []byte
	CreatedAt  string
	UpdatedAt  string
}

// SaveImportedModel inserts or replaces a model definition.
func (s *Store) SaveImportedModel(ctx context.Context, id string, definition []byte) error {
	t := nowISO()
	_, err := s.DB.ExecContext(ctx, `INSERT INTO imported_models (id, definition, created_at, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET definition = excluded.definition, updated_at = excluded.updated_at`, id, string(definition), t, t)
	return err
}

// ImportedModels lists stored definitions by id.
func (s *Store) ImportedModels(ctx context.Context) ([]ImportedModel, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, definition, created_at, updated_at FROM imported_models ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ImportedModel
	for rows.Next() {
		var m ImportedModel
		var def string
		if err := rows.Scan(&m.ID, &def, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		m.Definition = []byte(def)
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteImportedModel removes a definition and every device's choice for it.
func (s *Store) DeleteImportedModel(ctx context.Context, id string) (bool, error) {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM imported_models WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	_, err = s.DB.ExecContext(ctx, `DELETE FROM model_backend_selections WHERE model_id = ?`, id)
	return n > 0, err
}

// BackendChoice is a stored manual selection.
type BackendChoice struct {
	BackendID string
	VariantID string
	UpdatedAt string
}

// BackendChoice returns this device's manual choice for a model, or nil
// (automatic mode).
func (s *Store) BackendChoice(ctx context.Context, modelID, deviceID string) (*BackendChoice, error) {
	var c BackendChoice
	var variant sql.NullString
	err := s.DB.QueryRowContext(ctx, `SELECT backend_id, variant_id, updated_at FROM model_backend_selections WHERE model_id = ? AND device_id = ?`,
		modelID, deviceID).Scan(&c.BackendID, &variant, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	c.VariantID = variant.String
	return &c, err
}

// SetBackendChoice stores a manual choice for this device.
func (s *Store) SetBackendChoice(ctx context.Context, modelID, deviceID, backendID, variantID string) error {
	var variant any
	if variantID != "" {
		variant = variantID
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO model_backend_selections (model_id, device_id, backend_id, variant_id, updated_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT (model_id, device_id) DO UPDATE SET backend_id = excluded.backend_id,
		variant_id = excluded.variant_id, updated_at = excluded.updated_at`, modelID, deviceID, backendID, variant, nowISO())
	return err
}

// ResetBackendChoice returns a model to automatic mode on this device.
func (s *Store) ResetBackendChoice(ctx context.Context, modelID, deviceID string) (bool, error) {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM model_backend_selections WHERE model_id = ? AND device_id = ?`, modelID, deviceID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
