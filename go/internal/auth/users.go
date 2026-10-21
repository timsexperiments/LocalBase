package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"regexp"
	"sort"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Managed users (users.ts): invited by email with roles; pending until the
// first sign-in, then active; disabled or removed by an admin.

// User is a managed user.
type User struct {
	ID        string   `json:"id"`
	Email     string   `json:"email"`
	Status    string   `json:"status"`
	Roles     []string `json:"roles"`
	CreatedAt string   `json:"createdAt"`
	UpdatedAt string   `json:"updatedAt"`
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func userByEmail(ctx context.Context, q queryer, email string) (*User, error) {
	var u User
	err := q.QueryRowContext(ctx, `SELECT u.id, e.email, u.status, u.created_at, u.updated_at FROM auth_user_emails e
		JOIN auth_users u ON e.user_id = u.id WHERE e.email = ?`, email).Scan(&u.ID, &u.Email, &u.Status, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &AccessError{"managed-user-not-found", "Managed user " + email + " was not found."}
	}
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT r.name FROM auth_user_roles ur JOIN auth_roles r ON ur.role_id = r.id WHERE ur.user_id = ?`, u.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	u.Roles = []string{}
	for rows.Next() {
		var n string
		rows.Scan(&n)
		u.Roles = append(u.Roles, n)
	}
	sort.Strings(u.Roles)
	return &u, rows.Err()
}

// ListUsers lists managed users that are not removed, by email.
func ListUsers(ctx context.Context, st *store.Store) ([]User, error) {
	rows, err := st.DB.QueryContext(ctx, `SELECT e.email FROM auth_user_emails e JOIN auth_users u ON e.user_id = u.id
		WHERE u.status != 'removed' ORDER BY e.email`)
	if err != nil {
		return nil, err
	}
	var emails []string
	for rows.Next() {
		var e string
		rows.Scan(&e)
		emails = append(emails, e)
	}
	rows.Close()
	out := []User{}
	for _, e := range emails {
		u, err := userByEmail(ctx, st.DB, e)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, nil
}

func requirePolicy(ctx context.Context, q queryer) error {
	var id string
	if err := q.QueryRowContext(ctx, `SELECT id FROM auth_settings WHERE id = 'default'`).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return &AccessError{"policy-not-configured", "Configure a browser access policy first."}
	} else {
		return err
	}
}

func roleIDs(ctx context.Context, q queryer, roles []string) (map[string]string, error) {
	ids := map[string]string{}
	var missing []string
	for _, r := range roles {
		var id string
		if err := q.QueryRowContext(ctx, `SELECT id FROM auth_roles WHERE name = ?`, r).Scan(&id); err != nil {
			missing = append(missing, r)
			continue
		}
		ids[r] = id
	}
	if len(missing) > 0 {
		return nil, &AccessError{"role-not-found", "Unknown browser access role: " + joinComma(missing) + "."}
	}
	return ids, nil
}

func joinComma(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}

func validRoles(roles []string) error {
	seen := map[string]bool{}
	if len(roles) > 64 {
		return errors.New("roles: at most 64")
	}
	for _, r := range roles {
		if !roleNameRe.MatchString(r) || len(r) > 64 || seen[r] {
			return errors.New("Role names must be unique.")
		}
		seen[r] = true
	}
	return nil
}

// InviteUser creates (or re-invites a removed) managed user as pending.
func InviteUser(ctx context.Context, st *store.Store, email string, roles []string) (*User, error) {
	if email = ValidEmail(email); email == "" || len(roles) == 0 || validRoles(roles) != nil {
		return nil, errors.New("validation_failed")
	}
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := requirePolicy(ctx, tx); err != nil {
		return nil, err
	}
	ids, err := roleIDs(ctx, tx, roles)
	if err != nil {
		return nil, err
	}
	var userID, status string
	err = tx.QueryRowContext(ctx, `SELECT e.user_id, u.status FROM auth_user_emails e JOIN auth_users u ON e.user_id = u.id WHERE e.email = ?`, email).
		Scan(&userID, &status)
	exists := err == nil
	if exists && status != "removed" {
		return nil, &AccessError{"managed-user-exists", "Managed user " + email + " already exists."}
	}
	ts := now()
	if exists {
		tx.ExecContext(ctx, `UPDATE auth_users SET status = 'pending', updated_at = ? WHERE id = ?`, ts, userID)
		tx.ExecContext(ctx, `DELETE FROM auth_user_roles WHERE user_id = ?`, userID)
	} else {
		userID = store.UUID()
		if _, err := tx.ExecContext(ctx, `INSERT INTO auth_users (id, status, created_at, updated_at) VALUES (?, 'pending', ?, ?)`, userID, ts, ts); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO auth_user_emails (user_id, email) VALUES (?, ?)`, userID, email); err != nil {
			return nil, err
		}
	}
	for _, r := range roles {
		if _, err := tx.ExecContext(ctx, `INSERT INTO auth_user_roles (user_id, role_id) VALUES (?, ?)`, userID, ids[r]); err != nil {
			return nil, err
		}
	}
	u, err := userByEmail(ctx, tx, email)
	if err != nil {
		return nil, err
	}
	return u, tx.Commit()
}

// ReplaceUserRoles sets a managed user's roles.
func ReplaceUserRoles(ctx context.Context, st *store.Store, email string, roles []string) (*User, error) {
	if email = ValidEmail(email); email == "" || validRoles(roles) != nil {
		return nil, errors.New("validation_failed")
	}
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := requirePolicy(ctx, tx); err != nil {
		return nil, err
	}
	u, err := userByEmail(ctx, tx, email)
	if err != nil {
		return nil, err
	}
	if u.Status == "removed" {
		return nil, &AccessError{"managed-user-not-found", "Managed user " + email + " was not found."}
	}
	ids, err := roleIDs(ctx, tx, roles)
	if err != nil {
		return nil, err
	}
	tx.ExecContext(ctx, `DELETE FROM auth_user_roles WHERE user_id = ?`, u.ID)
	for _, r := range roles {
		if _, err := tx.ExecContext(ctx, `INSERT INTO auth_user_roles (user_id, role_id) VALUES (?, ?)`, u.ID, ids[r]); err != nil {
			return nil, err
		}
	}
	if u, err = userByEmail(ctx, tx, email); err != nil {
		return nil, err
	}
	return u, tx.Commit()
}

// SetUserStatus enables (active) or disables a managed user.
func SetUserStatus(ctx context.Context, st *store.Store, email, status string) (*User, error) {
	if email = ValidEmail(email); email == "" {
		return nil, errors.New("validation_failed")
	}
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	u, err := userByEmail(ctx, tx, email)
	if err != nil {
		return nil, err
	}
	if u.Status == "removed" {
		return nil, &AccessError{"managed-user-not-found", "Managed user " + email + " was not found."}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE auth_users SET status = ?, updated_at = ? WHERE id = ?`, status, now(), u.ID); err != nil {
		return nil, err
	}
	if u, err = userByEmail(ctx, tx, email); err != nil {
		return nil, err
	}
	return u, tx.Commit()
}

// RemoveUser marks a user removed and drops its roles and magic link. It
// returns the user as it was before removal.
func RemoveUser(ctx context.Context, st *store.Store, email string) (*User, error) {
	if email = ValidEmail(email); email == "" {
		return nil, errors.New("validation_failed")
	}
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	u, err := userByEmail(ctx, tx, email)
	if err != nil {
		return nil, err
	}
	if u.Status == "removed" {
		return nil, &AccessError{"managed-user-not-found", "Managed user " + email + " was not found."}
	}
	tx.ExecContext(ctx, `DELETE FROM auth_magic_link_tokens WHERE user_id = ?`, u.ID)
	tx.ExecContext(ctx, `DELETE FROM auth_user_roles WHERE user_id = ?`, u.ID)
	if _, err := tx.ExecContext(ctx, `UPDATE auth_users SET status = 'removed', updated_at = ? WHERE id = ?`, now(), u.ID); err != nil {
		return nil, err
	}
	return u, tx.Commit()
}

// --- sessions ---------------------------------------------------------------------

const (
	SessionIdleTTL      = 7 * 24 * time.Hour
	SessionAbsoluteTTL  = 30 * 24 * time.Hour
	sessionTouchEvery   = time.Hour
	MagicLinkTTL        = 15 * time.Minute
	magicLinkCooldown   = time.Minute
	sessionCookieMaxAge = int(SessionAbsoluteTTL / time.Second)
)

// Session is an `auth_browser_sessions` row (times in epoch ms).
type Session struct {
	OwnerID   string
	Identity  Identity
	CreatedAt int64
	LastSeen  int64
	IdleExp   int64
	AbsExp    int64
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// RandomToken is base64url of n random bytes.
func RandomToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func ms(t time.Time) int64 { return t.UnixMilli() }

// CreateSession stores a session and returns its cookie token.
func CreateSession(ctx context.Context, st *store.Store, ownerID string, id Identity) (string, error) {
	t := time.Now()
	st.DB.ExecContext(ctx, `DELETE FROM auth_browser_sessions WHERE idle_expires_at <= ? OR absolute_expires_at <= ?`, ms(t), ms(t))
	token := RandomToken(32)
	var email any
	if id.VerifiedEmail != "" {
		email = id.VerifiedEmail
	}
	_, err := st.DB.ExecContext(ctx, `INSERT INTO auth_browser_sessions (token_hash, owner_id, issuer, subject, verified_email,
		created_at, last_seen_at, idle_expires_at, absolute_expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		hashToken(token), ownerID, id.Issuer, id.Subject, email, ms(t), ms(t), ms(t.Add(SessionIdleTTL)), ms(t.Add(SessionAbsoluteTTL)))
	return token, err
}

// FindSession returns a live session for a cookie token, touching its idle
// expiry at most hourly; expired sessions are deleted.
func FindSession(ctx context.Context, st *store.Store, token string, issuers map[string]bool) (*Session, error) {
	h := hashToken(token)
	t := ms(time.Now())
	s, verdict, err := checkSession(ctx, st, h, issuers, t)
	switch {
	case err != nil:
		return nil, err
	case verdict == "expired" || verdict == "issuer":
		st.DB.ExecContext(ctx, `DELETE FROM auth_browser_sessions WHERE token_hash = ?`, h)
		return nil, nil
	case verdict == "valid-touch":
		st.DB.ExecContext(ctx, `UPDATE auth_browser_sessions SET last_seen_at = ?, idle_expires_at = ? WHERE token_hash = ?`,
			t, t+SessionIdleTTL.Milliseconds(), h)
	}
	return s, nil
}

// checkSession validates a stored session by its token hash at time t,
// without changing it (TS authenticate). The verdict is missing, issuer
// (not an accepted issuer), expired, valid, or valid-touch (valid and due
// for a last-seen update).
func checkSession(ctx context.Context, st *store.Store, h string, issuers map[string]bool, t int64) (*Session, string, error) {
	var s Session
	var email sql.NullString
	err := st.DB.QueryRowContext(ctx, `SELECT owner_id, issuer, subject, verified_email, created_at, last_seen_at, idle_expires_at,
		absolute_expires_at FROM auth_browser_sessions WHERE token_hash = ?`, h).
		Scan(&s.OwnerID, &s.Identity.Issuer, &s.Identity.Subject, &email, &s.CreatedAt, &s.LastSeen, &s.IdleExp, &s.AbsExp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "missing", nil
	}
	if err != nil {
		return nil, "", err
	}
	s.Identity.VerifiedEmail = email.String
	switch {
	case !issuers[s.Identity.Issuer]:
		return nil, "issuer", nil
	case s.IdleExp <= t || s.AbsExp <= t:
		return nil, "expired", nil
	case t-s.LastSeen >= sessionTouchEvery.Milliseconds():
		return &s, "valid-touch", nil
	}
	return &s, "valid", nil
}

// DeleteSession signs a cookie token out.
func DeleteSession(ctx context.Context, st *store.Store, token string) {
	st.DB.ExecContext(ctx, `DELETE FROM auth_browser_sessions WHERE token_hash = ?`, hashToken(token))
}

// --- magic links --------------------------------------------------------------------

var magicTokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// MagicIssuer is the identity issuer of magic-link sign-ins.
func MagicIssuer(origin string) string { return origin + "/magic-link" }

// IssueMagicLink creates a one-time token for an invited or active user,
// at most once a minute; nil when the address cannot sign in.
func IssueMagicLink(ctx context.Context, st *store.Store, email string) (token, address string, err error) {
	if email = ValidEmail(email); email == "" {
		return "", "", nil
	}
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	var userID, status string
	if err := tx.QueryRowContext(ctx, `SELECT u.id, u.status FROM auth_user_emails e JOIN auth_users u ON e.user_id = u.id WHERE e.email = ?`, email).
		Scan(&userID, &status); err != nil || (status != "pending" && status != "active") {
		return "", "", nil
	}
	var created string
	if tx.QueryRowContext(ctx, `SELECT created_at FROM auth_magic_link_tokens WHERE user_id = ?`, userID).Scan(&created) == nil {
		if t, err := time.Parse(time.RFC3339Nano, created); err == nil && time.Since(t) < magicLinkCooldown {
			return "", "", nil
		}
	}
	token = RandomToken(32)
	n := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO auth_magic_link_tokens (user_id, token_hash, expires_at, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET token_hash = excluded.token_hash, created_at = excluded.created_at, expires_at = excluded.expires_at`,
		userID, hashToken(token), n.Add(MagicLinkTTL).Format("2006-01-02T15:04:05.000Z"), n.Format("2006-01-02T15:04:05.000Z")); err != nil {
		return "", "", err
	}
	return token, email, tx.Commit()
}

// RevokeMagicLink drops a token (after a failed delivery).
func RevokeMagicLink(ctx context.Context, st *store.Store, token string) {
	if magicTokenRe.MatchString(token) {
		st.DB.ExecContext(ctx, `DELETE FROM auth_magic_link_tokens WHERE token_hash = ?`, hashToken(token))
	}
}

// ConsumeMagicLink redeems a token once; nil when invalid or expired.
func ConsumeMagicLink(ctx context.Context, st *store.Store, token, issuer string) (*Identity, error) {
	if !magicTokenRe.MatchString(token) {
		return nil, nil
	}
	h := hashToken(token)
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var userID, expires, status, email string
	err = tx.QueryRowContext(ctx, `SELECT t.user_id, t.expires_at, u.status, e.email FROM auth_magic_link_tokens t
		JOIN auth_users u ON t.user_id = u.id JOIN auth_user_emails e ON t.user_id = e.user_id WHERE t.token_hash = ?`, h).
		Scan(&userID, &expires, &status, &email)
	tx.ExecContext(ctx, `DELETE FROM auth_magic_link_tokens WHERE token_hash = ?`, h)
	if cerr := tx.Commit(); cerr != nil {
		return nil, cerr
	}
	if err != nil || (status != "pending" && status != "active") {
		return nil, nil
	}
	if t, perr := time.Parse(time.RFC3339Nano, expires); perr != nil || !t.After(time.Now()) {
		return nil, nil
	}
	return &Identity{Issuer: issuer, Subject: userID, VerifiedEmail: email}, nil
}

// ClearMagicLinks drops every pending token (serve start without magic link).
func ClearMagicLinks(ctx context.Context, st *store.Store) {
	st.DB.ExecContext(ctx, `DELETE FROM auth_magic_link_tokens`)
}
