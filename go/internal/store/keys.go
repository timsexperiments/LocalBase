package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
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

func (s *Store) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, name, prefix, key_hash, scopes, created_at, last_rotated_at, expires_at, revoked_at FROM api_keys ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		var scopes string
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.KeyHash, &scopes, &k.CreatedAt, &k.LastRotatedAt, &k.ExpiresAt, &k.RevokedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(scopes), &k.Scopes); err != nil {
			return nil, fmt.Errorf("api key %s: invalid scopes: %w", k.ID, err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ResolveAPIKey returns the active key whose hash matches, comparing in constant time.
func (s *Store) ResolveAPIKey(ctx context.Context, presented string) (*APIKey, error) {
	if presented == "" {
		return nil, nil
	}
	hash := HashKey(presented)
	keys, err := s.ListAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for i := range keys {
		if keys[i].Active(now) && subtle.ConstantTimeCompare([]byte(keys[i].KeyHash), []byte(hash)) == 1 {
			return &keys[i], nil
		}
	}
	return nil, nil
}

// CreateAPIKey inserts a key and returns the record plus the raw secret, shown once.
func (s *Store) CreateAPIKey(ctx context.Context, name string, scopes []string) (*APIKey, string, error) {
	if strings.TrimSpace(name) == "" {
		return nil, "", fmt.Errorf("API key name must not be empty")
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return nil, "", err
	}
	body := base64.RawURLEncoding.EncodeToString(buf)
	raw := "lb_" + body
	idBytes := make([]byte, 16)
	_, _ = rand.Read(idBytes)
	idBytes[6] = idBytes[6]&0x0f | 0x40
	idBytes[8] = idBytes[8]&0x3f | 0x80
	h := hex.EncodeToString(idBytes)
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	k := &APIKey{
		ID:   "key_" + h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:],
		Name: name, Prefix: body[:8], KeyHash: HashKey(raw), Scopes: scopes,
		CreatedAt: now, LastRotatedAt: now,
	}
	scopesJSON, _ := json.Marshal(scopes)
	_, err := s.DB.ExecContext(ctx, `INSERT INTO api_keys (id, name, prefix, key_hash, scopes, created_at, last_rotated_at, expires_at, revoked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULL, NULL)`, k.ID, k.Name, k.Prefix, k.KeyHash, string(scopesJSON), k.CreatedAt, k.LastRotatedAt)
	if err != nil {
		return nil, "", err
	}
	return k, raw, nil
}
