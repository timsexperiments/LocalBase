package gateway

import (
	"net/http"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

// A --no-auth gateway binds loopback (cmd/localbase), but that alone does not
// keep it private: any web page in a browser on this machine can send it
// requests, a page can rebind its own name to 127.0.0.1, and a tunnel on this
// machine connects from loopback. So it only answers requests that name this
// machine, did not come through a proxy, and, if from a browser, came from its
// own pages (DESIGN §13.6). --no-auth is for local development only.

// noAuthRefusal is why a --no-auth gateway refuses this request ("": it doesn't).
// A request with no Host at all is not from a browser, nor from a rebinding page.
func noAuthRefusal(r *http.Request) string {
	if r.Host != "" && !auth.LoopbackHost(r.Host) {
		return "This gateway runs with --no-auth, so it only answers requests addressed to this computer (127.0.0.1 or localhost)."
	}
	if auth.Proxied(r) {
		return "This gateway runs with --no-auth, which is for local development only and must never be put behind a proxy or tunnel."
	}
	if noAuthAPIPath(r.URL.Path) && !auth.SameOriginRequest(r) {
		return "This gateway runs with --no-auth, so its API only answers its own pages and clients that are not browsers."
	}
	return ""
}

// noAuthAPIPath: the routes that act with no credentials. A link from another
// site to a page such as /app stays a normal navigation; the pages' own actions
// have their same-origin checks.
func noAuthAPIPath(p string) bool {
	return strings.HasPrefix(p, "/v1/") || strings.HasPrefix(p, "/_localbase/") ||
		strings.HasPrefix(p, "/app/api/") || p == "/app/session"
}
