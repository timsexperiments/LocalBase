package store

import (
	"context"
	"time"
)

// ObservedPeak is a `model_observed_memory` row: the highest footprint this
// machine saw while serving a model, keyed by artifact digest and launch shape.
type ObservedPeak struct {
	ModelID        string
	ArtifactSHA256 string
	LaunchShape    string
	PeakBytes      int64
	Method         string // "footprint" (macOS) or "vmhwm" (Linux)
	ObservedAt     string
}

func (s *Store) ObservedPeaks(ctx context.Context, modelID, sha string) ([]ObservedPeak, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT model_id, artifact_sha256, launch_shape, peak_bytes, method, observed_at
		FROM model_observed_memory WHERE model_id = ? AND artifact_sha256 = ? ORDER BY observed_at DESC`, modelID, sha)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ObservedPeak
	for rows.Next() {
		var p ObservedPeak
		if err := rows.Scan(&p.ModelID, &p.ArtifactSHA256, &p.LaunchShape, &p.PeakBytes, &p.Method, &p.ObservedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RecordObservedPeak keeps the larger of the stored and the new peak.
func (s *Store) RecordObservedPeak(ctx context.Context, p ObservedPeak) error {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	_, err := s.DB.ExecContext(ctx, `INSERT INTO model_observed_memory (model_id, artifact_sha256, launch_shape, peak_bytes, method, observed_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (model_id, artifact_sha256, launch_shape) DO UPDATE SET
			peak_bytes = excluded.peak_bytes, method = excluded.method, observed_at = excluded.observed_at
		WHERE excluded.peak_bytes > model_observed_memory.peak_bytes`,
		p.ModelID, p.ArtifactSHA256, p.LaunchShape, p.PeakBytes, p.Method, now)
	return err
}
