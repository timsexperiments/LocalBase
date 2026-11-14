package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/uuidv4"
)

// APIKey is an `api_keys` row. The raw key (`lb_` + 32 base64url chars) is
// never stored; KeyHash is its sha256 hex.
type APIKey struct {
	ID            string
	Name          string
	Prefix        string
	KeyHash       string
	Scopes        []string
	CreatedAt     string
	LastRotatedAt string
	ExpiresAt     *string
	RevokedAt     *string
}

func HashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func (k *APIKey) Active(now time.Time) bool {
	if k.RevokedAt != nil {
		return false
	}
	if k.ExpiresAt == nil {
		return true
	}
	t, err := time.Parse(time.RFC3339Nano, *k.ExpiresAt)
	return err == nil && t.After(now)
}

// keyRowColumns are what scanAPIKey reads, in its order.
const keyRowColumns = `id, name, prefix, key_hash, scopes, created_at, last_rotated_at, expires_at, revoked_at`

// scanAPIKey reads one row of keyRowColumns.
func scanAPIKey(scan func(...any) error) (APIKey, error) {
	var k APIKey
	var scopes string
	if err := scan(&k.ID, &k.Name, &k.Prefix, &k.KeyHash, &scopes, &k.CreatedAt, &k.LastRotatedAt, &k.ExpiresAt, &k.RevokedAt); err != nil {
		return k, err
	}
	if err := json.Unmarshal([]byte(scopes), &k.Scopes); err != nil {
		return k, fmt.Errorf("api key %s: invalid scopes: %w", k.ID, err)
	}
	return k, nil
}

func (s *Store) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+keyRowColumns+` FROM api_keys ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ResolveAPIKey returns the active key whose hash matches. It reads that key's own row, so another row that
// can't be read doesn't stop it working. The raw key is random, so finding it by its hash tells a caller
// nothing a guess could use.
func (s *Store) ResolveAPIKey(ctx context.Context, presented string) (*APIKey, error) {
	if presented == "" {
		return nil, nil
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+keyRowColumns+` FROM api_keys WHERE key_hash = ? ORDER BY created_at, id`, HashKey(presented))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := time.Now()
	for rows.Next() {
		k, err := scanAPIKey(rows.Scan)
		if err != nil {
			return nil, err
		}
		if k.Active(now) {
			return &k, nil
		}
	}
	return nil, rows.Err()
}

// CreateAPIKey inserts a key and returns the record plus the raw secret, shown once.
func (s *Store) CreateAPIKey(ctx context.Context, name string, scopes []string) (*APIKey, string, error) {
	return s.CreateAPIKeyExpiring(ctx, name, scopes, 0)
}

// CreateAPIKeyExpiring is CreateAPIKey with an expiry in days (0 = never).
func (s *Store) CreateAPIKeyExpiring(ctx context.Context, name string, scopes []string, expiresDays int) (*APIKey, string, error) {
	if strings.TrimSpace(name) == "" {
		return nil, "", fmt.Errorf("API key name must not be empty")
	}
	if expiresDays < 0 {
		return nil, "", fmt.Errorf("API key expiry must be a positive whole number of days.")
	}
	raw, prefix := newRawKey()
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	k := &APIKey{ID: "key_" + UUID(), Name: name, Prefix: prefix, KeyHash: HashKey(raw), Scopes: scopes,
		CreatedAt: now, LastRotatedAt: now}
	if expiresDays > 0 {
		exp := time.Now().Add(time.Duration(expiresDays) * 24 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
		k.ExpiresAt = &exp
	}
	scopesJSON, _ := json.Marshal(scopes)
	_, err := s.DB.ExecContext(ctx, `INSERT INTO api_keys (id, name, prefix, key_hash, scopes, created_at, last_rotated_at, expires_at, revoked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)`, k.ID, k.Name, k.Prefix, k.KeyHash, string(scopesJSON), k.CreatedAt, k.LastRotatedAt, k.ExpiresAt)
	if err != nil {
		return nil, "", err
	}
	return k, raw, nil
}

// UUID returns a random version 4 UUID: uuidv4.New.
func UUID() string { return uuidv4.New() }

// newRawKey is `lb_` + base64url(24 random bytes); the prefix is the first
// eight characters after `lb_`.
func newRawKey() (raw, prefix string) {
	buf := make([]byte, 24)
	_, _ = rand.Read(buf)
	body := base64.RawURLEncoding.EncodeToString(buf)
	return "lb_" + body, body[:8]
}

// ErrKeyNotFound is returned for an unknown key id.
type ErrKeyNotFound struct{ ID string }

func (e *ErrKeyNotFound) Error() string { return "API key not found: " + e.ID }

// APIKeyByID returns a key or ErrKeyNotFound.
func (s *Store) APIKeyByID(ctx context.Context, id string) (*APIKey, error) {
	keys, err := s.ListAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	for i := range keys {
		if keys[i].ID == id {
			return &keys[i], nil
		}
	}
	return nil, &ErrKeyNotFound{id}
}

func (s *Store) RevokeAPIKey(ctx context.Context, id string) (*APIKey, error) {
	k, err := s.APIKeyByID(ctx, id)
	if err != nil {
		return nil, err
	}
	now := Now()
	if _, err := s.DB.ExecContext(ctx, `UPDATE api_keys SET revoked_at = ? WHERE id = ?`, now, id); err != nil {
		return nil, err
	}
	k.RevokedAt = &now
	return k, nil
}

// RotateAPIKey replaces the secret and returns the new raw key.
func (s *Store) RotateAPIKey(ctx context.Context, id string) (*APIKey, string, error) {
	k, err := s.APIKeyByID(ctx, id)
	if err != nil {
		return nil, "", err
	}
	raw, prefix := newRawKey()
	k.Prefix, k.KeyHash, k.LastRotatedAt = prefix, HashKey(raw), Now()
	_, err = s.DB.ExecContext(ctx, `UPDATE api_keys SET prefix = ?, key_hash = ?, last_rotated_at = ? WHERE id = ?`,
		k.Prefix, k.KeyHash, k.LastRotatedAt, id)
	return k, raw, err
}

// SetAPIKeyScopes replaces a key's scopes (already canonical).
func (s *Store) SetAPIKeyScopes(ctx context.Context, id string, scopes []string) (*APIKey, error) {
	k, err := s.APIKeyByID(ctx, id)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(scopes)
	if _, err := s.DB.ExecContext(ctx, `UPDATE api_keys SET scopes = ? WHERE id = ?`, string(b), id); err != nil {
		return nil, err
	}
	k.Scopes = scopes
	return k, nil
}

// DeleteAPIKey removes a key outright (keys the CLI minted for itself).
func (s *Store) DeleteAPIKey(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, id)
	return err
}
