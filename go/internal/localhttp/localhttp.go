// Package localhttp is the HTTP client for talking to a LocalBase gateway on this machine: the
// gateway an owner record names, which may carry the instance token and whose answers say what
// the root is doing.
//
// The client goes to that address and nowhere else:
//   - no proxy: http.DefaultTransport honours HTTP_PROXY and HTTPS_PROXY for any host that isn't
//     loopback, and an owner record may name one (a gateway bound to a LAN address). The token
//     would pass through the proxy, and the proxy could answer for the gateway;
//   - no redirect: a redirect is not followed, and the caller gets the 3xx answer as it came
//     (http.ErrUseLastResponse). Go copies a custom header like the token onto a followed
//     request, even to another host, and a gateway never redirects these routes.
//
// Clients to anywhere else (downloads, sign-in providers, the update check) want the proxy and
// have their own redirect rules, so they don't use this.
package localhttp

import (
	"net/http"
	"time"
)

// transport is DefaultTransport's settings with no proxy, shared so that repeated probes reuse
// connections instead of each leaving its own idle ones behind.
var transport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	return t
}()

// MaxAnswer is the most of a local gateway's answer a caller reads: its admin answers are small,
// and a larger one is refused rather than read whole. instance and upgrade both read to it.
const MaxAnswer = 1 << 20

// New returns a client with no proxy that follows no redirect. timeout is the client's whole-call
// limit, as http.Client.Timeout; 0 leaves the limit to the request's context.
func New(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
