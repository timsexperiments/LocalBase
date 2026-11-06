package store

import "context"

// Voice is a `tts_custom_voices` row (#366).
type Voice struct {
	ID, Name, NameKey, SHA256     string
	SizeBytes, DurationMs         int64
	CreatedAt, CreatedBy          string
	ConsentStatement, ConsentedAt string
}

// Voices lists stored custom voices, oldest first.
func (s *Store) Voices(ctx context.Context) ([]Voice, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, name, name_key, sha256, size_bytes, duration_ms, created_at, created_by,
		consent_statement, consent_attested_at FROM tts_custom_voices ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Voice
	for rows.Next() {
		var v Voice
		if err := rows.Scan(&v.ID, &v.Name, &v.NameKey, &v.SHA256, &v.SizeBytes, &v.DurationMs, &v.CreatedAt, &v.CreatedBy,
			&v.ConsentStatement, &v.ConsentedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
