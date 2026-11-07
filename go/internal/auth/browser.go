package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Browser sign-in (ui/access.ts, ui/oidc-session.ts): Cloudflare Access JWT
// assertions, or direct OIDC (authorization code + PKCE + nonce) and magic
// links with database sessions. Cookie names, session rows and owner ids
// match TS, so sessions survive a switch between the implementations.

const (
	loginStateCookie  = "__Host-localbase-login-state"
	SessionCookie     = "__Host-localbase-session"
	loginStateTTL     = 10 * time.Minute
	loginStateRandom  = 24
	maxSpentStates    = 4096
	magicLinkWorkers  = 4
	maxIdPResponse    = 64 * 1024
	OIDCCallbackPath  = "/oidc/callback"
	MagicCallbackPath = "/magic-link/callback"
	MagicRequestPath  = "/app/login/magic"
)

var (
	allowedAlgs   = []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA"}
	cookieValueRe = regexp.MustCompile(`^[A-Za-z0-9_-]{32,128}$`)
	asciiSubRe    = regexp.MustCompile(`^[\x00-\x7F]{1,255}$`)
)

// Browser is the browser-identity layer of one gateway process. The access
// configuration is read at startup; changes need a restart, as in TS.
type Browser struct {
	Store  *store.Store
	Root   string
	Config *BrowserAccess // nil: browser access off
	Log    *slog.Logger
	Client *http.Client

	cfJWKS *JWKS
	oidc   map[string]*oidcRuntime
	mu     sync.Mutex
	// stateKey signs login states. It is made at start and held only in memory, so it is
	// in no file and no backup, and a restart ends a sign-in in progress, as it always did.
	stateKey []byte
	// spent holds the states of callbacks being worked on or finished, until they would
	// have expired anyway.
	spent map[string]time.Time
	// magicWork bounds the magic-link requests being worked on, since anyone can post one.
	magicWork chan struct{}
	// magicDropped is when a dropped request was last logged, as Unix seconds.
	magicDropped atomic.Int64
	// magicFailed is when a link that could not be issued was last logged, as Unix seconds.
	magicFailed atomic.Int64
	// failedLog keeps what failed sign-ins write to the log small.
	failedLog *obs.Sampler
}

// NewBrowser prepares sign-in for a loaded configuration.
func NewBrowser(st *store.Store, rootDir string, cfg *BrowserAccess, log *slog.Logger) *Browser {
	b := &Browser{
		Store:  st,
		Root:   rootDir,
		Config: cfg,
		Log:    log,
		Client: &http.Client{
			Timeout:       5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects are not followed") },
		},
		oidc:      map[string]*oidcRuntime{},
		stateKey:  make([]byte, 32),
		spent:     map[string]time.Time{},
		magicWork: make(chan struct{}, magicLinkWorkers),
		failedLog: &obs.Sampler{Every: time.Minute, Burst: 20},
	}
	if _, err := rand.Read(b.stateKey); err != nil {
		panic(err) // never, since Go 1.24
	}
	if cfg == nil {
		return b
	}
	if cfg.Provider.Kind == "cloudflare-access" {
		b.cfJWKS = &JWKS{URL: "https://" + cfg.Provider.TeamDomain + "/cdn-cgi/access/certs", Client: b.Client}
	}
	for _, r := range cfg.Provider.Registrations {
		if r.Kind == "oidc" {
			b.oidc[r.ID] = &oidcRuntime{reg: r}
		}
	}
	return b
}

// Enabled reports whether browser sign-in is configured.
func (b *Browser) Enabled() bool { return b != nil && b.Config != nil }

// Direct reports whether sign-in uses LocalBase sessions (OIDC, magic link).
func (b *Browser) Direct() bool { return b.Enabled() && b.Config.Provider.Kind == "direct" }

func ownerID(parts ...string) string {
	j, _ := json.Marshal(parts)
	sum := sha256.Sum256(j)
	return "browser:" + hex.EncodeToString(sum[:])
}

func (b *Browser) sessionIssuers() map[string]bool {
	out := map[string]bool{}
	for _, r := range b.Config.Provider.Registrations {
		switch r.Kind {
		case "oidc":
			out[r.Issuer] = true
		case "magic-link":
			out[MagicIssuer(b.Config.Origin)] = true
		}
	}
	return out
}

func cookie(r *http.Request, name string) string {
	var found []string
	for _, c := range r.Cookies() {
		if c.Name == name {
			found = append(found, c.Value)
		}
	}
	if len(found) != 1 || !cookieValueRe.MatchString(found[0]) {
		return ""
	}
	return found[0]
}

// Authenticate resolves the request's browser identity, if any.
func (b *Browser) Authenticate(r *http.Request) (*Identity, string) {
	id, owner, _ := b.AuthenticateChecked(r)
	return id, owner
}

// AuthenticateChecked is Authenticate, and says when a session couldn't be read: that isn't the same
// as having no session, and shouldn't send a signed-in person to sign in again.
func (b *Browser) AuthenticateChecked(r *http.Request) (*Identity, string, error) {
	if !b.Enabled() {
		return nil, "", nil
	}
	if b.Config.Provider.Kind == "cloudflare-access" {
		return b.cloudflareIdentity(r)
	}
	token := cookie(r, SessionCookie)
	if token == "" {
		return nil, "", nil
	}
	s, err := FindSession(r.Context(), b.Store, token, b.sessionIssuers())
	if err != nil {
		return nil, "", err
	}
	if s == nil {
		return nil, "", nil
	}
	return &s.Identity, s.OwnerID, nil
}

// Authorize resolves an identity's permissions: the policy when one is
// configured, else the configuration's permissions.
func (b *Browser) Authorize(ctx context.Context, id *Identity) *Decision {
	d, err := b.AuthorizeChecked(ctx, id)
	if err != nil {
		return &Decision{MatchedRoles: []string{}, Permissions: []string{}}
	}
	return d
}

// AuthorizeChecked is Authorize, and says when the policy couldn't be read: that isn't the same as the
// policy granting nothing.
func (b *Browser) AuthorizeChecked(ctx context.Context, id *Identity) (*Decision, error) {
	d, err := ResolvePolicy(ctx, b.Store, *id, true)
	if err != nil {
		b.Log.Error("Browser access policy lookup failed.", "event", "auth.policy-failed", "error", err.Error())
		return nil, err
	}
	if d == nil {
		return &Decision{MatchedRoles: []string{}, Permissions: b.Config.Permissions}, nil
	}
	return d, nil
}

// --- responses ------------------------------------------------------------------------

func redirect(w http.ResponseWriter, location string, cookies ...*http.Cookie) {
	for _, c := range cookies {
		http.SetCookie(w, c)
	}
	w.Header().Set("Location", location)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusSeeOther)
}

func secureCookie(name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode}
}

func clearCookie(name string) *http.Cookie {
	c := secureCookie(name, "", 0)
	c.MaxAge = -1
	return c
}

// Deny is the TS UI access failure body.
func Deny(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write([]byte(`{"error":{"message":"UI access denied.","type":"authentication_error","code":"ui_access_denied"}}`))
}

// Logout serves POST /app/logout.
func (b *Browser) Logout(w http.ResponseWriter, r *http.Request) {
	if b.Direct() {
		if token := cookie(r, SessionCookie); token != "" {
			if err := DeleteSession(r.Context(), b.Store, token); err != nil {
				// The session is still good, so the person isn't told otherwise and keeps the cookie.
				b.Log.Error("Could not end a browser session; it stays valid until it expires.", "event", "auth.session-delete-failed", "error", err.Error())
				w.Header().Set("Retry-After", "5")
				Deny(w, http.StatusServiceUnavailable)
				return
			}
		}
		http.SetCookie(w, clearCookie(SessionCookie))
	}
	w.WriteHeader(http.StatusNoContent)
}
