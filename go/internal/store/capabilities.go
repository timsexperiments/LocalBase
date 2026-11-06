package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// RuntimeCapabilities is a `model_runtime_capabilities` row: what a running
// backend reported for one exact artifact.
type RuntimeCapabilities struct {
	ModelID         string
	ArtifactSHA256  string
	Capabilities    map[string]bool
	InputModalities []string
	BuildInfo       string
	ObservedAt      string
}

// RuntimeCapabilitiesFor returns nil when nothing was recorded.
func (s *Store) RuntimeCapabilitiesFor(ctx context.Context, modelID, sha string) (*RuntimeCapabilities, error) {
	var caps, mods string
	r := RuntimeCapabilities{ModelID: modelID, ArtifactSHA256: sha}
	err := s.DB.QueryRowContext(ctx, `SELECT capabilities, input_modalities, build_info, observed_at
		FROM model_runtime_capabilities WHERE model_id = ? AND artifact_sha256 = ?`, modelID, sha).
		Scan(&caps, &mods, &r.BuildInfo, &r.ObservedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if json.Unmarshal([]byte(caps), &r.Capabilities) != nil {
		r.Capabilities = map[string]bool{}
	}
	if json.Unmarshal([]byte(mods), &r.InputModalities) != nil {
		r.InputModalities = []string{"text"}
	}
	return &r, nil
}

func (s *Store) RecordRuntimeCapabilities(ctx context.Context, r RuntimeCapabilities) error {
	caps, _ := json.Marshal(r.Capabilities)
	mods, _ := json.Marshal(r.InputModalities)
	if r.ObservedAt == "" {
		r.ObservedAt = Now()
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO model_runtime_capabilities
		(model_id, artifact_sha256, capabilities, input_modalities, build_info, observed_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (model_id, artifact_sha256) DO UPDATE SET capabilities = excluded.capabilities,
			input_modalities = excluded.input_modalities, build_info = excluded.build_info, observed_at = excluded.observed_at`,
		r.ModelID, r.ArtifactSHA256, string(caps), string(mods), r.BuildInfo, r.ObservedAt)
	return err
}

// Now is the ISO-8601 UTC timestamp format TS stores (Date.toISOString).
func Now() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }
