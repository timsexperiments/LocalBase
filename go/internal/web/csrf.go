package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// CSRF for state-changing htmx requests (operator review of §15.3), on top
// of the same-origin checks in sameOriginUI: a random per-browser-session
// cookie (HttpOnly, SameSite=Strict) and a token derived from it with a
// per-root key, which the layout puts in htmx's headers. A cross-site form
// can send neither the header nor a matching token.

const (
	csrfCookie = "lb_csrf"
	csrfHeader = "X-Csrf-Token"
)

var (
	csrfOnce sync.Map // root dir -> *csrfKey
)

type csrfKey struct {
	once sync.Once
	key  []byte
}

// csrfSecret is the root's key (runtime/ui-csrf.key, 0600), created on first
// use so tokens survive restarts; in memory without a root.
func (u *UI) csrfSecret() []byte {
	v, _ := csrfOnce.LoadOrStore(u.d.RootDir, &csrfKey{})
	k := v.(*csrfKey)
	k.once.Do(func() {
		if u.d.RootDir != "" {
			path := filepath.Join(u.d.RootDir, "runtime", "ui-csrf.key")
			if b, err := os.ReadFile(path); err == nil && len(b) == 32 {
				k.key = b
				return
			}
			b := make([]byte, 32)
			rand.Read(b)
			os.MkdirAll(filepath.Dir(path), 0o700)
			if os.WriteFile(path, b, 0o600) == nil {
				k.key = b
				return
			}
		}
		k.key = make([]byte, 32)
		rand.Read(k.key)
	})
	return k.key
}

func (u *UI) csrfFor(session string) string {
	m := hmac.New(sha256.New, u.csrfSecret())
	m.Write([]byte(session))
	return hex.EncodeToString(m.Sum(nil))
}

// csrfToken returns the page's token, starting a session cookie if needed.
func (u *UI) csrfToken(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(csrfCookie); err == nil && len(c.Value) >= 32 {
		return u.csrfFor(c.Value)
	}
	b := make([]byte, 32)
	rand.Read(b)
	session := base64.RawURLEncoding.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: session, Path: "/app", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"})
	r.AddCookie(&http.Cookie{Name: csrfCookie, Value: session}) // later renders in this request agree
	return u.csrfFor(session)
}

// csrfValid reports a request carrying its session's token.
func (u *UI) csrfValid(r *http.Request) bool {
	c, err := r.Cookie(csrfCookie)
	if err != nil || len(c.Value) < 32 {
		return false
	}
	return hmac.Equal([]byte(r.Header.Get(csrfHeader)), []byte(u.csrfFor(c.Value)))
}
