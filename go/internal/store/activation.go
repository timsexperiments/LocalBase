package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"

	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
)

// Declarative configuration's pending-restart record (TS activation.ts).
// The listener and the memory policy are read once at startup, so saving a
// change to them records the new settings' fingerprint in
// config_activation; a managed gateway that becomes ready with exactly
// those settings clears it. The fingerprint is the TS one, so either
// implementation reads the other's record.

// StaticFingerprint is sha256 of JSON.stringify([gatewayHost, gatewayPort,
// system percent, system minimumGb, accelerator percent, accelerator
// minimumGb]).
func StaticFingerprint(c *Config) string {
	doc := "[" + string(jsjson.Stringify(c.GatewayHost)) + "," + strconv.Itoa(c.GatewayPort) + "," +
		jsjson.Number(c.SystemReserve.Percent) + "," + jsjson.Number(c.SystemReserve.MinimumGB) + "," +
		jsjson.Number(c.AcceleratorReserve.Percent) + "," + jsjson.Number(c.AcceleratorReserve.MinimumGB) + "]"
	sum := sha256.Sum256([]byte(doc))
	return hex.EncodeToString(sum[:])
}

type queryer interface {
	execer
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// savedStatic is the persisted config's fingerprint, or "" without a row.
func savedStatic(ctx context.Context, db queryer) (string, error) {
	c := &Config{}
	err := db.QueryRowContext(ctx, `SELECT gateway_host, gateway_port, memory_system_reserve_percent,
		memory_system_reserve_minimum_gb, memory_accelerator_reserve_percent, memory_accelerator_reserve_minimum_gb
		FROM config WHERE id = 'default'`).Scan(&c.GatewayHost, &c.GatewayPort, &c.SystemReserve.Percent,
		&c.SystemReserve.MinimumGB, &c.AcceleratorReserve.Percent, &c.AcceleratorReserve.MinimumGB)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return StaticFingerprint(c), nil
}

func markPending(ctx context.Context, db execer, c *Config) error {
	_, err := db.ExecContext(ctx, `INSERT INTO config_activation (id, pending_static_config) VALUES ('default', ?)
		ON CONFLICT (id) DO UPDATE SET pending_static_config = excluded.pending_static_config`, StaticFingerprint(c))
	return err
}

// SaveConfigMarking saves the config and, in the same transaction, records
// a pending restart when the listener or memory policy changed (TS
// persistConfiguration).
func (s *Store) SaveConfigMarking(ctx context.Context, c *Config) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	previous, err := savedStatic(ctx, tx)
	if err != nil {
		return err
	}
	if err := saveConfig(ctx, tx, c); err != nil {
		return err
	}
	if previous != StaticFingerprint(c) {
		if err := markPending(ctx, tx, c); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MarkRestartPending records c's static settings as awaiting a restart.
func (s *Store) MarkRestartPending(ctx context.Context, c *Config) error {
	return markPending(ctx, s.DB, c)
}

// MarkRestartPendingUnlessChanged re-marks c after a failed restart, unless
// the saved settings have moved on since.
func (s *Store) MarkRestartPendingUnlessChanged(ctx context.Context, c *Config) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	saved, err := savedStatic(ctx, tx)
	if err != nil {
		return err
	}
	if saved == StaticFingerprint(c) {
		if err := markPending(ctx, tx, c); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RestartPending reports whether saved static settings await a restart.
func (s *Store) RestartPending(ctx context.Context) (bool, error) {
	var id string
	err := s.DB.QueryRowContext(ctx, `SELECT id FROM config_activation WHERE id = 'default'`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// AcknowledgeStatic clears the pending restart when a managed gateway is
// ready with exactly the recorded settings, and they are still the saved
// ones (TS acknowledgeStaticConfiguration).
func (s *Store) AcknowledgeStatic(ctx context.Context, started *Config) error {
	fp := StaticFingerprint(started)
	var pending string
	err := s.DB.QueryRowContext(ctx, `SELECT pending_static_config FROM config_activation WHERE id = 'default'`).Scan(&pending)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && pending != fp) {
		return nil
	}
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if saved, err := savedStatic(ctx, tx); err != nil || saved != fp {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM config_activation WHERE id = 'default' AND pending_static_config = ?`, fp); err != nil {
		return err
	}
	return tx.Commit()
}
