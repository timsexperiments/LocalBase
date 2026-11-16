package auth

import (
	"net/http"
	"net/url"
	"strings"
)

// ExactHost: the request is for the configured origin's host.
func (b *Browser) ExactHost(r *http.Request) bool {
	u, _ := url.Parse(b.Config.Origin)
	return r.Host == u.Host
}

// SameOrigin is the common CSRF rule for state-changing and UI API calls:
// the UI marker header, the exact host, a same-origin fetch, and the exact
// Origin on anything but GET.
func (b *Browser) SameOrigin(r *http.Request) bool {
	if !b.Enabled() || r.Header.Get("X-Localbase-Ui") != "1" || !b.ExactHost(r) {
		return false
	}
	site, origin := r.Header.Get("Sec-Fetch-Site"), r.Header.Get("Origin")
	if site != "" && site != "same-origin" {
		return false
	}
	if origin != "" && origin != b.Config.Origin {
		return false
	}
	return r.Method == http.MethodGet || origin == b.Config.Origin
}

func formOK(r *http.Request, origin string) bool {
	site := r.Header.Get("Sec-Fetch-Site")
	return r.Header.Get("Origin") == origin && (site == "" || site == "same-origin") &&
		strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/x-www-form-urlencoded")
}

// SameOriginStream is SameOrigin for a GET event stream (DESIGN §21.3). An
// EventSource cannot add X-Localbase-Ui, so it rests on the session cookie,
// the exact host, and the browser's same-origin markers. It is read-only:
// without CORS headers no other site can read the stream.
func (b *Browser) SameOriginStream(r *http.Request) bool {
	if !b.Enabled() || r.Method != http.MethodGet || !b.ExactHost(r) {
		return false
	}
	site, origin := r.Header.Get("Sec-Fetch-Site"), r.Header.Get("Origin")
	if site != "" && site != "same-origin" {
		return false
	}
	return origin == "" || origin == b.Config.Origin
}
