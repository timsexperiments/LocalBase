package auth

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// One-time local sign-in (DESIGN §28). Whoever can run the CLI on this root
// already controls it, so the CLI may hand that person a browser session, as long
// as it can only be used from this computer. A session therefore has every
// permission on purpose, and is not an identity anyone else can see or manage.
const (
	LocalCookie      = "localbase-local-session"
	LocalOncePath    = "/app/login/once"
	LocalOwnerID     = "local"
	LocalLinkTTL     = 10 * time.Minute
	LocalIdleTTL     = 12 * time.Hour
	LocalAbsoluteTTL = 7 * 24 * time.Hour
	// MaxLocalSessions is how many may exist; a new one ends the least recently used.
	MaxLocalSessions = 5
)

// LocalRedeemScript posts the token from the URL fragment, which never reaches a
// server log and which a link preview cannot use up. Pages pin it by hash.
const LocalRedeemScript = `const t=location.hash.slice(1);history.replaceState(null,"",location.pathname);const f=document.getElementById("redeem");f.elements.token.value=t;f.submit();`

// LocalRequest is the loopback rule, checked on redeeming and on every later
// request. The socket alone is not enough: a tunnel or reverse proxy on this
// machine also connects from loopback, so the Host and the absence of proxy
// markers are what exclude a remote visitor. Host also stops DNS rebinding.
func LocalRequest(r *http.Request) bool {
	if Proxied(r) || !LoopbackHost(r.Host) {
		return false
	}
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	if ip := net.ParseIP(remote); ip == nil || !ip.IsLoopback() {
		return false
	}
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return false
	}
	tcp, ok := local.(*net.TCPAddr)
	if !ok || !tcp.IP.IsLoopback() {
		return false
	}
	// The Host must name the port this request actually arrived on.
	_, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		port = "80"
	}
	return port == strconv.Itoa(tcp.Port)
}

// LocalSameOrigin is the CSRF rule for a local session's UI calls: the rule
// SameOrigin applies, with this request's own loopback origin.
func LocalSameOrigin(r *http.Request) bool {
	if r.Header.Get("X-Localbase-Ui") != "1" {
		return false
	}
	return localOrigin(r) && (r.Method == http.MethodGet || r.Header.Get("Origin") != "")
}

// LocalSameOriginStream is LocalSameOrigin for a GET event stream, which cannot
// add a header (SameOriginStream).
func LocalSameOriginStream(r *http.Request) bool {
	return r.Method == http.MethodGet && localOrigin(r)
}

func localOrigin(r *http.Request) bool {
	site, origin := r.Header.Get("Sec-Fetch-Site"), r.Header.Get("Origin")
	return (site == "" || site == "same-origin") && (origin == "" || origin == "http://"+r.Host)
}

// LocalFormOK is the check on the redeem form's POST.
func LocalFormOK(r *http.Request) bool { return formOK(r, "http://"+r.Host) }

// ReadLocalToken reads the redeem form's one field.
func ReadLocalToken(r *http.Request) (string, bool) {
	t, ok := readForm(r, 256, "token")
	return t, ok && magicTokenRe.MatchString(t)
}

// LocalLinkID names a token in logs: never the token.
func LocalLinkID(token string) string { return hashToken(token)[:8] }

// IssueLocalLink creates a link and returns its token, replacing any unused one.
// Only the token's hash is stored.
func IssueLocalLink(ctx context.Context, st *store.Store) (string, error) {
	token := RandomToken(32)
	t := time.Now()
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM local_login_links`); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO local_login_links (token_hash, created_at, expires_at) VALUES (?, ?, ?)`,
		hashToken(token), ms(t), ms(t.Add(LocalLinkTTL))); err != nil {
		return "", err
	}
	return token, tx.Commit()
}

// ConsumeLocalLink spends a token. A wrong, used or expired one reports false,
// all alike. There is at most one live link, so the comparison is in constant
// time against it, and the delete is what makes a double submit win only once.
func ConsumeLocalLink(ctx context.Context, st *store.Store, token string) (bool, error) {
	var stored string
	err := st.DB.QueryRowContext(ctx, `SELECT token_hash FROM local_login_links LIMIT 1`).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(hashToken(token))) != 1 {
		return false, nil
	}
	res, err := st.DB.ExecContext(ctx, `DELETE FROM local_login_links WHERE token_hash = ? AND expires_at > ?`, stored, ms(time.Now()))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// LocalSession is a session's public record: never its token.
type LocalSession struct {
	ID         string `json:"id"`
	CreatedAt  int64  `json:"createdAt"`
	LastSeenAt int64  `json:"lastSeenAt"`
	ExpiresAt  int64  `json:"expiresAt"`
}

// CreateLocalSession stores a session and returns its cookie token and id, ending
// the least recently used beyond MaxLocalSessions.
func CreateLocalSession(ctx context.Context, st *store.Store) (token, id string, err error) {
	t := time.Now()
	token, id = RandomToken(32), RandomToken(9)
	st.DB.ExecContext(ctx, `DELETE FROM local_sessions WHERE idle_expires_at <= ? OR absolute_expires_at <= ?`, ms(t), ms(t))
	_, err = st.DB.ExecContext(ctx, `INSERT INTO local_sessions (id, token_hash, created_at, last_seen_at, idle_expires_at, absolute_expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`, id, hashToken(token), ms(t), ms(t), ms(t.Add(LocalIdleTTL)), ms(t.Add(LocalAbsoluteTTL)))
	if err != nil {
		return "", "", err
	}
	_, err = st.DB.ExecContext(ctx, `DELETE FROM local_sessions WHERE id NOT IN
		(SELECT id FROM local_sessions ORDER BY last_seen_at DESC, created_at DESC, id LIMIT ?)`, MaxLocalSessions)
	return token, id, err
}

// FindLocalSession returns the live session for a cookie token, moving its idle
// expiry at most every sessionTouchEvery; never past its absolute expiry.
func FindLocalSession(ctx context.Context, st *store.Store, token string) (*LocalSession, error) {
	h := hashToken(token)
	t := ms(time.Now())
	var s LocalSession
	var idle, abs int64
	err := st.DB.QueryRowContext(ctx, `SELECT id, created_at, last_seen_at, idle_expires_at, absolute_expires_at FROM local_sessions
		WHERE token_hash = ?`, h).Scan(&s.ID, &s.CreatedAt, &s.LastSeenAt, &idle, &abs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if idle <= t || abs <= t {
		st.DB.ExecContext(ctx, `DELETE FROM local_sessions WHERE token_hash = ?`, h)
		return nil, nil
	}
	if t-s.LastSeenAt >= sessionTouchEvery.Milliseconds() {
		st.DB.ExecContext(ctx, `UPDATE local_sessions SET last_seen_at = ?, idle_expires_at = ? WHERE token_hash = ?`,
			t, t+LocalIdleTTL.Milliseconds(), h)
		s.LastSeenAt, idle = t, t+LocalIdleTTL.Milliseconds()
	}
	s.ExpiresAt = min(idle, abs)
	return &s, nil
}

// DeleteLocalSession signs a cookie token out.
func DeleteLocalSession(ctx context.Context, st *store.Store, token string) {
	st.DB.ExecContext(ctx, `DELETE FROM local_sessions WHERE token_hash = ?`, hashToken(token))
}

// ListLocalSessions lists live sessions, most recently used first.
func ListLocalSessions(ctx context.Context, st *store.Store) ([]LocalSession, error) {
	t := ms(time.Now())
	rows, err := st.DB.QueryContext(ctx, `SELECT id, created_at, last_seen_at, MIN(idle_expires_at, absolute_expires_at) FROM local_sessions
		WHERE idle_expires_at > ? AND absolute_expires_at > ? ORDER BY last_seen_at DESC, created_at DESC, id`, t, t)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LocalSession{}
	for rows.Next() {
		var s LocalSession
		if err := rows.Scan(&s.ID, &s.CreatedAt, &s.LastSeenAt, &s.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RevokeLocalSession ends one session by id, reporting whether it existed.
func RevokeLocalSession(ctx context.Context, st *store.Store, id string) (bool, error) {
	res, err := st.DB.ExecContext(ctx, `DELETE FROM local_sessions WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// RevokeAllLocal ends every local session and unused link.
func RevokeAllLocal(ctx context.Context, st *store.Store) (int, error) {
	if _, err := st.DB.ExecContext(ctx, `DELETE FROM local_login_links`); err != nil {
		return 0, err
	}
	res, err := st.DB.ExecContext(ctx, `DELETE FROM local_sessions`)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// LocalSessionCookie is host-only and not Secure: Safari rejects a Secure cookie
// on http://127.0.0.1, and plain HTTP on loopback does not leave the machine.
func LocalSessionCookie(token string) *http.Cookie {
	return &http.Cookie{Name: LocalCookie, Value: token, Path: "/", MaxAge: int(LocalAbsoluteTTL.Seconds()), HttpOnly: true, SameSite: http.SameSiteStrictMode}
}

// ClearLocalSessionCookie removes it.
func ClearLocalSessionCookie() *http.Cookie {
	c := LocalSessionCookie("")
	c.MaxAge = -1
	return c
}

// LocalCookieToken reads the request's one session cookie.
func LocalCookieToken(r *http.Request) string { return cookie(r, LocalCookie) }

// Redirect is the browser layer's no-store redirect.
func Redirect(w http.ResponseWriter, location string, cookies ...*http.Cookie) {
	redirect(w, location, cookies...)
}
