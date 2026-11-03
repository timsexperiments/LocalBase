package main

import (
	"context"
	"net"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
)

// --no-auth gives anyone who can reach the gateway every permission, so it is
// only allowed when the gateway binds loopback. Only the gateway's listener
// needs checking: backends always bind loopback (runtime.BackendHost), and
// --no-auth is the only way to turn authentication off.

// lookupHost resolves a host name; tests replace it.
var lookupHost = func(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// hostIsLoopback reports whether binding host can only reach loopback
// addresses: 127.0.0.0/8, ::1, localhost, or a name that resolves only to
// those. An empty host binds every interface, so it is not.
func hostIsLoopback(host string) bool {
	h := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(host), "["), "]")
	if h == "" {
		return false
	}
	if strings.EqualFold(strings.TrimSuffix(h, "."), "localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ips, err := lookupHost(ctx, h)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return false
		}
	}
	return true
}

// noAuthHostError refuses a --no-auth serve that would bind beyond loopback.
func noAuthHostError(host string) error {
	if hostIsLoopback(host) {
		return nil
	}
	return cli.Inputf("Refusing --no-auth: the gateway would bind %q, which is not a loopback address, and without authentication anyone who can reach it gets full access. Bind loopback (--host 127.0.0.1), or remove --no-auth and use an API key.", host)
}

// noAuthListenerError is the last check, on the address really bound: a name
// can resolve differently between the check and the bind.
func noAuthListenerError(addr net.Addr) error {
	if tcp, ok := addr.(*net.TCPAddr); ok && tcp.IP.IsLoopback() {
		return nil
	}
	return cli.Inputf("Refusing --no-auth: the gateway bound %s, which is not a loopback address.", addr)
}
