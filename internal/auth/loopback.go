package auth

import (
	"net"
	"net/http"
	"strings"
)

// proxyHeaders mark a request that came through a proxy or tunnel. Such a
// request also arrives from a loopback socket when the proxy runs on this
// machine, so the socket alone says nothing about where the person is.
var proxyHeaders = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip",
	"Cf-Connecting-Ip", "Cf-Ray", "Cf-Access-Jwt-Assertion"}

// LoopbackHost reports whether an HTTP Host names this machine: localhost or a
// loopback address, with or without a port. Any other name is refused, which is
// what stops DNS rebinding.
func LoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Proxied reports whether a request carries any proxy or tunnel header.
func Proxied(r *http.Request) bool {
	for _, name := range proxyHeaders {
		if r.Header.Get(name) != "" {
			return true
		}
	}
	return false
}

// SameOriginRequest reports whether a browser, if one sent this, sent it from
// this page's own origin. Clients that are not browsers send neither header.
func SameOriginRequest(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	// "null", or anything without a host of this request's, is another origin.
	scheme, host, ok := strings.Cut(origin, "://")
	return ok && (scheme == "http" || scheme == "https") && strings.EqualFold(host, r.Host)
}
