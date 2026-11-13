package netguard

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestHostIsLoopback(t *testing.T) {
	names := map[string][]net.IP{
		"only-loopback.test": {net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		"mixed.test":         {net.IPv4(127, 0, 0, 1), net.IPv4(192, 168, 1, 9)},
		"lan.test":           {net.IPv4(192, 168, 1, 9)},
		"nothing.test":       {},
	}
	lookup := func(_ context.Context, host string) ([]net.IP, error) {
		if ips, ok := names[host]; ok {
			return ips, nil
		}
		return nil, errors.New("no such host")
	}
	for host, want := range map[string]bool{
		"127.0.0.1":          true,
		"127.1.2.3":          true,
		"::1":                true,
		"[::1]":              true,
		"localhost":          true,
		"LOCALHOST.":         true,
		"only-loopback.test": true,
		"":                   false, // binds every interface
		" ":                  false,
		"0.0.0.0":            false,
		"::":                 false,
		"[::]":               false,
		"192.168.1.9":        false,
		"10.0.0.1":           false,
		"fe80::1":            false,
		"mixed.test":         false, // one reachable address is enough
		"lan.test":           false,
		"nothing.test":       false,
		"unresolvable.test":  false,
	} {
		if got := hostIsLoopback(host, lookup); got != want {
			t.Errorf("HostIsLoopback(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestNoAuthListener(t *testing.T) {
	for addr, ok := range map[string]bool{"127.0.0.1:1": true, "[::1]:1": true, "0.0.0.0:1": false, "[::]:1": false, "192.168.1.9:1": false} {
		a, err := net.ResolveTCPAddr("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		if got := NoAuthListener(a) == nil; got != ok {
			t.Errorf("%s: allowed = %v, want %v", addr, got, ok)
		}
	}
	if NoAuthListener(&net.UnixAddr{Name: "/tmp/x"}) == nil {
		t.Error("a non-TCP listener was allowed")
	}
}

// Each refusal is a *NoAuthRefusal, in the words a person reads.
func TestNoAuthRefusalsSayWhy(t *testing.T) {
	var r *NoAuthRefusal
	if err := NoAuthHost("0.0.0.0"); !errors.As(err, &r) || r.Message != `Refusing --no-auth: the gateway would bind "0.0.0.0", which is not a loopback address, and without authentication anyone who can reach it gets full access. Bind loopback (--host 127.0.0.1), or remove --no-auth and use an API key.` {
		t.Errorf("host: %v", err)
	}
	a, _ := net.ResolveTCPAddr("tcp", "192.168.1.9:1")
	if err := NoAuthListener(a); !errors.As(err, &r) || r.Message != "Refusing --no-auth: the gateway bound 192.168.1.9:1, which is not a loopback address." {
		t.Errorf("listener: %v", err)
	}
}

// HostIsLoopback asks the system resolver: a name that can't resolve (.invalid never does) is
// not loopback, and localhost is, as a name and resolved.
func TestHostIsLoopbackResolvesForReal(t *testing.T) {
	if HostIsLoopback("no-such-host.invalid") {
		t.Error("an unresolvable name counted as loopback")
	}
	if !HostIsLoopback("localhost") {
		t.Error("localhost")
	}
	if NoAuthHost("no-such-host.invalid") == nil {
		t.Error("--no-auth was allowed on an unresolvable name")
	}
}
