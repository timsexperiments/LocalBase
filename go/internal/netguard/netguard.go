// Package netguard decides which network addresses a download may reach. The check
// runs when a connection is made, on the address actually being dialled, so it holds
// across redirects and for a name that resolves differently than it did earlier.
package netguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Class is what an address is to a download.
type Class int

const (
	// Public addresses are always allowed.
	Public Class = iota
	// Private addresses are this machine and its networks: allowed only by opt-in.
	Private
	// Never addresses are allowed by nothing: link-local and cloud metadata
	// services, multicast, and addresses that are not real destinations. No LAN
	// mirror lives there, and metadata services hand out credentials.
	Never
)

func prefixes(list ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(list))
	for i, s := range list {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

var (
	never = prefixes(
		"0.0.0.0/8",          // "this network"
		"169.254.0.0/16",     // link-local, including 169.254.169.254
		"192.0.0.0/24",       // IETF protocol assignments, including 192.0.0.192
		"192.0.2.0/24",       // documentation
		"198.18.0.0/15",      // benchmarking
		"198.51.100.0/24",    // documentation
		"203.0.113.0/24",     // documentation
		"224.0.0.0/4",        // multicast
		"240.0.0.0/4",        // reserved, including broadcast
		"100.100.100.200/32", // Alibaba Cloud metadata
		"168.63.129.16/32",   // Azure wire server, in a public range
		"::/128",             // unspecified
		"fe80::/10",          // link-local
		"ff00::/8",           // multicast
		"2001::/32",          // Teredo: carries an IPv4 address it can reach
		"2001:db8::/32",      // documentation
		"2002::/16",          // 6to4: carries an IPv4 address
		"fd00:ec2::254/128",  // AWS metadata
	)
	private = prefixes(
		"10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
		"::1/128", "fc00::/7",
	)
	// nat64 carries an IPv4 address in its last 32 bits, and is how an IPv6-only
	// network reaches every public IPv4 site, so it is judged by what it carries.
	nat64 = netip.MustParsePrefix("64:ff9b::/96")
	// compat is the deprecated IPv4-compatible form: never a real destination.
	compat = netip.MustParsePrefix("::/96")
)

// Classify says what an address is.
func Classify(ip netip.Addr) Class {
	ip = ip.WithZone("").Unmap()
	if !ip.IsValid() {
		return Never
	}
	if nat64.Contains(ip) {
		b := ip.As16()
		return Classify(netip.AddrFrom4([4]byte(b[12:16])))
	}
	if ip.Is6() && compat.Contains(ip) && !ip.IsLoopback() {
		return Never
	}
	for _, p := range never {
		if p.Contains(ip) {
			return Never
		}
	}
	for _, p := range private {
		if p.Contains(ip) {
			return Private
		}
	}
	return Public
}

// BlockedError is a refused destination.
type BlockedError struct {
	Host  string
	Class Class
}

func (e *BlockedError) Error() string {
	if e.Class == Never {
		return "refusing to connect to " + e.Host + ": downloads never use link-local, metadata or reserved addresses"
	}
	return "refusing to connect to " + e.Host + ": it is a private or local address"
}

// Policy says what a client may reach.
type Policy struct {
	// AllowPrivate is the explicit opt-in to private addresses, for a LAN mirror.
	AllowPrivate bool
	// AllowHosts are hosts the person running LocalBase named themselves, in the
	// environment or the configuration: a Hugging Face mirror, a feed. They are
	// matched by the name in the URL and may be private. Keep them out of reach of
	// anything a request can set.
	AllowHosts []string
	// Resolver is for tests (nil: the default).
	Resolver *net.Resolver
}

func (p Policy) allowsHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, h := range p.AllowHosts {
		if strings.TrimSuffix(strings.ToLower(h), ".") == host {
			return true
		}
	}
	return false
}

func (p Policy) check(host string, ip netip.Addr) error {
	switch class := Classify(ip); {
	case class == Public, p.allowsHost(host):
		return nil
	case class == Private && p.AllowPrivate:
		return nil
	default:
		return &BlockedError{Host: host, Class: class}
	}
}

// looseIPv4 reads the forms an address can take besides dotted decimal: fewer parts
// (127.1), hexadecimal (0x7f.0.0.1), octal (0177.0.0.1) and one number (2130706433).
// Some resolvers and proxies accept them, so they must not pass as names.
func looseIPv4(host string) (netip.Addr, bool) {
	parts := strings.Split(host, ".")
	if len(parts) > 4 {
		return netip.Addr{}, false
	}
	vals := make([]uint64, len(parts))
	for i, p := range parts {
		v, err := strconv.ParseUint(p, 0, 64)
		if err != nil {
			return netip.Addr{}, false
		}
		vals[i] = v
	}
	var n uint64
	for i, v := range vals {
		if i == len(vals)-1 {
			if v >= 1<<(8*(5-uint(len(vals)))) {
				return netip.Addr{}, false
			}
			n |= v
		} else {
			if v > 255 {
				return netip.Addr{}, false
			}
			n |= v << (24 - 8*uint(i))
		}
	}
	return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}), true
}

// literal is a host written as an address, in any form, if it is one.
func literal(host string) (netip.Addr, bool) {
	host = strings.Trim(host, "[]")
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip, true
	}
	return looseIPv4(strings.TrimSuffix(host, "."))
}

var localSuffixes = []string{"localhost", "local", "internal", "intranet", "lan", "home.arpa"}

func localName(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, s := range localSuffixes {
		if host == s || strings.HasSuffix(host, "."+s) {
			return true
		}
	}
	return false
}

// CheckURL refuses a destination that can be judged without connecting: an address
// in any form, or a name that only exists locally. This is what protects a request
// going through a proxy, which resolves the name itself.
func (p Policy) CheckURL(u *url.URL) error {
	host := u.Hostname()
	if p.allowsHost(host) {
		return nil
	}
	if ip, ok := literal(host); ok {
		return p.check(host, ip)
	}
	if localName(host) && !p.AllowPrivate {
		return &BlockedError{Host: host, Class: Private}
	}
	return nil
}

// proxyHosts are the proxies this process is configured to use: connecting to one is
// not a request for what lies behind it, which CheckURL has already judged.
func proxyHosts() []string {
	var out []string
	for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		v := os.Getenv(name)
		if v == "" {
			continue
		}
		if !strings.Contains(v, "://") {
			v = "http://" + v
		}
		if u, err := url.Parse(v); err == nil && u.Hostname() != "" {
			out = append(out, u.Hostname())
		}
	}
	return out
}

// DialContext dials with the check on every address tried. It sees the address
// that was resolved, so a name that now points somewhere else is judged by where
// it points.
func (p Policy) DialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	proxies := proxyHosts()
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		pass := p
		pass.AllowHosts = append(append([]string{}, p.AllowHosts...), proxies...)
		var blocked error
		d := net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Resolver: p.Resolver,
			Control: func(_, address string, _ syscall.RawConn) error {
				ap, err := netip.ParseAddrPort(address)
				if err != nil {
					blocked = &BlockedError{Host: host, Class: Never}
					return blocked
				}
				if err := pass.check(host, ap.Addr()); err != nil {
					blocked = err
					return err
				}
				return nil
			}}
		conn, err := d.DialContext(ctx, network, addr)
		if err != nil && blocked != nil {
			return nil, blocked
		}
		return conn, err
	}
}

// Transport returns a copy of base that keeps to the policy.
func (p Policy) Transport(base *http.Transport) http.RoundTripper {
	t := base.Clone()
	t.DialContext = p.DialContext()
	t.DialTLSContext = nil
	return &guardedTransport{base: t, policy: p}
}

type guardedTransport struct {
	base   http.RoundTripper
	policy Policy
}

func (g *guardedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := g.policy.CheckURL(r.URL); err != nil {
		return nil, err
	}
	return g.base.RoundTrip(r)
}

// Client returns a client that keeps to the policy, with the default transport's
// settings and the default redirect limit.
func (p Policy) Client() *http.Client {
	return &http.Client{Transport: p.Transport(http.DefaultTransport.(*http.Transport))}
}

// IsBlocked reports a refused destination in an error chain.
func IsBlocked(err error) bool {
	var b *BlockedError
	return errors.As(err, &b)
}
