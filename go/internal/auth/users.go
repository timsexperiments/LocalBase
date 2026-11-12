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
	"slices"
	"sort"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Managed users (users.ts): invited by email with roles; pending until the
// first sign-in, then active; disabled or removed by an admin.

// User is a managed user.
type User struct {
	ID     string   `json:"id"`
	Email  string   `json:"email"`
	Status string   `json:"status"`
	Roles  []string `json:"roles"`
	// Issuer is the sign-in provider that can claim this record.
	Issuer    string `json:"issuer,omitempty"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func userByEmail(ctx context.Context, q queryer, email string) (*User, error) {
	var u User
	err := q.QueryRowContext(ctx, `SELECT u.id, e.email, u.status, COALESCE(i.issuer, ''), u.created_at, u.updated_at FROM auth_user_emails e
		JOIN auth_users u ON e.user_id = u.id LEFT JOIN go_invitation_issuers i ON i.user_id = u.id WHERE e.email = ?`, email).
		Scan(&u.ID, &u.Email, &u.Status, &u.Issuer, &u.CreatedAt, &u.UpdatedAt)
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
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		u.Roles = append(u.Roles, n)
	}
	sort.Strings(u.Roles)
	return &u, rows.Err()
}

// ListUsers lists managed users that are not removed, by email.
func ListUsers(ctx context.Context, st *store.Store) ([]User, error) {
	return listUsers(ctx, st, false)
}

func listUsers(ctx context.Context, st *store.Store, removed bool) ([]User, error) {
	rows, err := st.DB.QueryContext(ctx, `SELECT e.email FROM auth_user_emails e JOIN auth_users u ON e.user_id = u.id
		WHERE ? OR u.status != 'removed' ORDER BY e.email`, removed)
	if err != nil {
		return nil, err
	}
	var emails []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			rows.Close()
			return nil, err
		}
		emails = append(emails, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
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

// InviteUser creates (or re-invites a removed) managed user as pending. Only a sign-in
// through issuer can claim the record; with none, nothing can until one is named.
func InviteUser(ctx context.Context, st *store.Store, email string, roles []string, issuer string) (*User, error) {
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
		if _, err := tx.ExecContext(ctx, `UPDATE auth_users SET status = 'pending', updated_at = ? WHERE id = ?`, ts, userID); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM auth_user_roles WHERE user_id = ?`, userID); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM go_invitation_issuers WHERE user_id = ?`, userID); err != nil {
			return nil, err
		}
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
	if issuer != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO go_invitation_issuers (user_id, issuer) VALUES (?, ?)`, userID, issuer); err != nil {
			return nil, err
		}
	}
	u, err := userByEmail(ctx, tx, email)
	if err != nil {
		return nil, err
	}
	return u, tx.Commit()
}

// ReplaceUserRoles sets a managed user's roles. changed says whether they were another set, from
// the read before the write (the write is made either way).
func ReplaceUserRoles(ctx context.Context, st *store.Store, email string, roles []string) (user *User, changed bool, err error) {
	if email = ValidEmail(email); email == "" || validRoles(roles) != nil {
		return nil, false, errors.New("validation_failed")
	}
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if err := requirePolicy(ctx, tx); err != nil {
		return nil, false, err
	}
	u, err := userByEmail(ctx, tx, email)
	if err != nil {
		return nil, false, err
	}
	if u.Status == "removed" {
		return nil, false, &AccessError{"managed-user-not-found", "Managed user " + email + " was not found."}
	}
	changed = !sameRoles(u.Roles, roles) // from the read above, before the write
	ids, err := roleIDs(ctx, tx, roles)
	if err != nil {
		return nil, false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_user_roles WHERE user_id = ?`, u.ID); err != nil {
		return nil, false, err // otherwise the old roles would stay beside the new ones
	}
	for _, r := range roles {
		if _, err := tx.ExecContext(ctx, `INSERT INTO auth_user_roles (user_id, role_id) VALUES (?, ?)`, u.ID, ids[r]); err != nil {
			return nil, false, err
		}
	}
	if u, err = userByEmail(ctx, tx, email); err != nil {
		return nil, false, err
	}
	return u, changed, tx.Commit()
}

// SetUserStatus enables (active) or disables a managed user. changed says whether its status
// was another, from the read before the write.
func SetUserStatus(ctx context.Context, st *store.Store, email, status string) (user *User, changed bool, err error) {
	if email = ValidEmail(email); email == "" {
		return nil, false, errors.New("validation_failed")
	}
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	u, err := userByEmail(ctx, tx, email)
	if err != nil {
		return nil, false, err
	}
	if u.Status == "removed" {
		return nil, false, &AccessError{"managed-user-not-found", "Managed user " + email + " was not found."}
	}
	changed = u.Status != status
	if _, err := tx.ExecContext(ctx, `UPDATE auth_users SET status = ?, updated_at = ? WHERE id = ?`, status, now(), u.ID); err != nil {
		return nil, false, err
	}
	if u, err = userByEmail(ctx, tx, email); err != nil {
		return nil, false, err
	}
	return u, changed, tx.Commit()
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
	for _, q := range []string{`DELETE FROM auth_magic_link_tokens WHERE user_id = ?`, `DELETE FROM auth_user_roles WHERE user_id = ?`, `DELETE FROM go_invitation_issuers WHERE user_id = ?`} {
		if _, err := tx.ExecContext(ctx, q, u.ID); err != nil {
			return nil, err
		}
	}
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
	// Tidying only: an expired row is refused when read, whether or not this removes it.
	if _, err := st.DB.ExecContext(ctx, `DELETE FROM auth_browser_sessions WHERE idle_expires_at <= ? OR absolute_expires_at <= ?`, ms(t), ms(t)); err != nil {
		st.WriteFailed("session-tidy", err)
	}
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
		// Refused either way; this only tidies the row, and the verdict comes again if it stays.
		if _, err := st.DB.ExecContext(ctx, `DELETE FROM auth_browser_sessions WHERE token_hash = ?`, h); err != nil {
			st.WriteFailed("session-tidy", err)
		}
		return nil, nil
	case verdict == "valid-touch":
		// A failed touch only makes the idle limit come sooner: it fails closed.
		if _, err := st.DB.ExecContext(ctx, `UPDATE auth_browser_sessions SET last_seen_at = ?, idle_expires_at = ? WHERE token_hash = ?`,
			t, t+SessionIdleTTL.Milliseconds(), h); err != nil {
			st.WriteFailed("session-touch", err) // still due, so the next request tries again
		}
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

// SignIn is someone with a live session.
type SignIn struct {
	Email    string `json:"email,omitempty"`
	Issuer   string `json:"issuer"`
	LastSeen string `json:"lastSeen"`
}

// SignedIn lists who has a live session, one line a person, the latest first.
func SignedIn(ctx context.Context, st *store.Store) ([]SignIn, error) {
	t := ms(time.Now())
	rows, err := st.DB.QueryContext(ctx, `SELECT COALESCE(MAX(verified_email), ''), MAX(issuer), MAX(last_seen_at) FROM auth_browser_sessions
		WHERE idle_expires_at > ? AND absolute_expires_at > ? GROUP BY owner_id ORDER BY MAX(last_seen_at) DESC LIMIT 100`, t, t)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SignIn{}
	for rows.Next() {
		var s SignIn
		var seen int64
		if err := rows.Scan(&s.Email, &s.Issuer, &seen); err != nil {
			return nil, err
		}
		s.LastSeen = time.UnixMilli(seen).UTC().Format(time.RFC3339)
		out = append(out, s)
	}
	return out, rows.Err()
}

// DeleteSession signs a cookie token out. A failure leaves that session valid until it
// expires, so the caller must hear about it.
func DeleteSession(ctx context.Context, st *store.Store, token string) error {
	_, err := st.DB.ExecContext(ctx, `DELETE FROM auth_browser_sessions WHERE token_hash = ?`, hashToken(token))
	return err
}

// --- magic links --------------------------------------------------------------------

var magicTokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// MagicIssuer is the identity issuer of magic-link sign-ins.
func MagicIssuer(origin string) string { return origin + "/magic-link" }

// IssueMagicLink creates a one-time token for an invited or active user who can sign
// in through issuer, unless their last link is still unused and unexpired; nil when
// the address cannot sign in, or no new link is due.
func IssueMagicLink(ctx context.Context, st *store.Store, email, issuer string) (token, address string, err error) {
	if email = ValidEmail(email); email == "" {
		return "", "", nil
	}
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	var userID, status string
	err = tx.QueryRowContext(ctx, `SELECT u.id, u.status FROM auth_user_emails e JOIN auth_users u ON e.user_id = u.id WHERE e.email = ?
		AND (EXISTS (SELECT 1 FROM go_invitation_issuers i WHERE i.user_id = u.id AND i.issuer = ?)
			OR EXISTS (SELECT 1 FROM auth_identities a WHERE a.user_id = u.id AND a.issuer = ?))`, email, issuer, issuer).
		Scan(&userID, &status)
	switch {
	case errors.Is(err, sql.ErrNoRows), err == nil && status != "pending" && status != "active":
		return "", "", nil
	case err != nil:
		return "", "", err
	}
	// A link that is still unused and unexpired stays the one that works, so a new request sends nothing:
	// asking can't replace the link the person was sent, or send them another every minute. A used link's
	// row is gone, and so is one that couldn't be delivered.
	var expires string
	switch err := tx.QueryRowContext(ctx, `SELECT expires_at FROM auth_magic_link_tokens WHERE user_id = ?`, userID).Scan(&expires); {
	case err == nil:
		if t, perr := time.Parse(time.RFC3339Nano, expires); perr == nil && t.After(time.Now()) {
			return "", "", nil
		}
	case !errors.Is(err, sql.ErrNoRows):
		return "", "", err
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
func RevokeMagicLink(ctx context.Context, st *store.Store, token string) error {
	if !magicTokenRe.MatchString(token) {
		return nil
	}
	_, err := st.DB.ExecContext(ctx, `DELETE FROM auth_magic_link_tokens WHERE token_hash = ?`, hashToken(token))
	return err
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
	// Used up first, or not at all: a link that could not be spent must not sign anyone in.
	if _, derr := tx.ExecContext(ctx, `DELETE FROM auth_magic_link_tokens WHERE token_hash = ?`, h); derr != nil {
		return nil, derr
	}
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
func ClearMagicLinks(ctx context.Context, st *store.Store) error {
	_, err := st.DB.ExecContext(ctx, `DELETE FROM auth_magic_link_tokens`)
	return err
}

// sameRoles says whether two role lists name the same roles, in any order.
func sameRoles(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(slices.Compact(x), slices.Compact(y))
}
