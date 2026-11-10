package gateway

import (
	"net/http"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
)

// principal resolves the caller for routes that do their own permission
// checks: a forwarded browser identity, --no-auth, or an API credential.
func (s *Server) principal(r *http.Request) *auth.Principal {
	if bp, ok := r.Context().Value(httpx.BrowserPrincipalKey{}).(*auth.Principal); ok {
		return bp
	}
	if s.cfg.NoAuth {
		return &auth.Principal{Kind: "environment", Permissions: auth.Permissions}
	}
	return s.resolveCredential(r)
}

// resolveCredential is every path from an API credential to a principal. A lookup that failed says
// nothing about the key, so the request is marked (BrowserPrincipal does the same for a session), and Unauthorized and Forbidden answer 503 for it:
// a client told 401 may treat a good key as revoked and throw it away.
func (s *Server) resolveCredential(r *http.Request) *auth.Principal {
	p, err := s.cfg.Auth.ResolveAPI(r.Context(), r)
	if err != nil {
		s.lookupFailed(r, err)
	}
	httpx.SetAuth(r, p)
	return p
}

// lookupFailed logs a failed look at a key or session, and marks the request.
func (s *Server) lookupFailed(r *http.Request, err error) {
	s.cfg.Log.Error("Credential lookup failed.", "event", "auth.lookup-failed", "error", err.Error())
	auth.MarkLookupFailed(r.Context())
}

// localCLI: this machine's CLI, holding the token from the root's lock file, on a request that
// passes the loopback rule (DESIGN §32.4). It is good for changes only.
func (s *Server) localCLI(r *http.Request) bool {
	if s.cfg.Root == nil || auth.IsBrowser(r) || !auth.LocalRequest(r) {
		return false
	}
	return httpx.InstanceTokenMatches(s.cfg.Root.Instance(), r)
}
