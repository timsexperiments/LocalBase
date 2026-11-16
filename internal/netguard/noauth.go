package netguard

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// --no-auth gives anyone who can reach the gateway every permission, so it is
// only allowed when the gateway binds loopback. Only the gateway's listener
// needs checking: backends always bind loopback (runtime.BackendHost), and
// --no-auth is the only way to turn authentication off.

// NoAuthRefusal is a --no-auth gateway that would reach beyond loopback. Message says so; a
// command line answers it as bad input.
type NoAuthRefusal struct{ Message string }

func (e *NoAuthRefusal) Error() string { return e.Message }

// HostIsLoopback reports whether binding host can only reach loopback
// addresses: 127.0.0.0/8, ::1, localhost, or a name that resolves only to
// those. An empty host binds every interface, so it is not.
func HostIsLoopback(host string) bool {
	return hostIsLoopback(host, func(ctx context.Context, name string) ([]net.IP, error) {
		return net.DefaultResolver.LookupIP(ctx, "ip", name)
	})
}

// hostIsLoopback is HostIsLoopback resolving names with lookup (tests: names that resolve as told).
func hostIsLoopback(host string, lookup func(ctx context.Context, host string) ([]net.IP, error)) bool {
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
	ips, err := lookup(ctx, h)
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

// NoAuthHost refuses a --no-auth gateway that would bind beyond loopback.
func NoAuthHost(host string) error {
	if HostIsLoopback(host) {
		return nil
	}
	return &NoAuthRefusal{fmt.Sprintf("Refusing --no-auth: the gateway would bind %q, which is not a loopback address, and without authentication anyone who can reach it gets full access. Bind loopback (--host 127.0.0.1), or remove --no-auth and use an API key.", host)}
}

// NoAuthListener is the last check, on the address really bound: a name
// can resolve differently between the check and the bind.
func NoAuthListener(addr net.Addr) error {
	if tcp, ok := addr.(*net.TCPAddr); ok && tcp.IP.IsLoopback() {
		return nil
	}
	return &NoAuthRefusal{fmt.Sprintf("Refusing --no-auth: the gateway bound %s, which is not a loopback address.", addr)}
}
