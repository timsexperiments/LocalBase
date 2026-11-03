package gateway

import (
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

// ConnLimits caps open connections (DESIGN §30.1). Zero means no cap. Connections
// from loopback do not count against PerIP: a tunnel or reverse proxy on this
// machine connects from loopback on behalf of everyone behind it.
type ConnLimits struct {
	Global int
	PerIP  int
}

// DefaultConnLimits leave room for a busy machine: a browser opens about six
// connections to a host, and a client pool a few dozen.
var DefaultConnLimits = ConnLimits{Global: 4096, PerIP: 256}

// CapForFileLimit lowers a global cap to fit under the process's open file limit,
// leaving room for logs, the database, model files and backends.
func CapForFileLimit(global int, fileLimit uint64) int {
	const reserve, floor = 128, 64
	if global <= 0 || fileLimit == 0 {
		return global
	}
	if fileLimit <= reserve+floor {
		return floor
	}
	if room := fileLimit - reserve; uint64(global) > room {
		return int(room)
	}
	return global
}

// busyResponse answers a connection over a cap. It is written directly, because
// nothing else has been read from the connection and no handler should run for it.
var busyResponse = func() string {
	body := `{"error":{"message":"Too many open connections. Try again shortly.","type":"api_error","param":null,"code":"too_busy"}}`
	return fmt.Sprintf("HTTP/1.1 503 Service Unavailable\r\nContent-Type: application/json\r\nRetry-After: 5\r\nConnection: close\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
}()

type limitListener struct {
	net.Listener
	limits ConnLimits
	log    *slog.Logger

	mu      sync.Mutex
	total   int
	perIP   map[string]int
	refused map[string]int // by reason, since the last log line
	logged  time.Time
}

// LimitConnections applies caps to a listener. A connection over a cap gets a 503
// and is closed at once, instead of waiting in the kernel's queue. With no caps it
// returns the listener unchanged.
func LimitConnections(ln net.Listener, l ConnLimits, log *slog.Logger) net.Listener {
	if l.Global <= 0 && l.PerIP <= 0 {
		return ln
	}
	return &limitListener{Listener: ln, limits: l, log: log, perIP: map[string]int{}, refused: map[string]int{}}
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		key := connKey(c.RemoteAddr())
		if reason := l.admit(key); reason != "" {
			c.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
			c.Write([]byte(busyResponse))
			c.Close()
			l.noteRefused(reason)
			continue
		}
		return &limitedConn{Conn: c, l: l, key: key}, nil
	}
}

// connKey groups connections by source: an IPv4 address, or an IPv6 /64, which one
// machine commonly holds whole. Loopback gets "", which PerIP does not count.
func connKey(addr net.Addr) string {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok || tcp.IP == nil || tcp.IP.IsLoopback() {
		return ""
	}
	if v4 := tcp.IP.To4(); v4 != nil {
		return v4.String()
	}
	return tcp.IP.Mask(net.CIDRMask(64, 128)).String()
}

func (l *limitListener) admit(key string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.limits.Global > 0 && l.total >= l.limits.Global {
		return "global"
	}
	if key != "" && l.limits.PerIP > 0 && l.perIP[key] >= l.limits.PerIP {
		return "per-ip"
	}
	l.total++
	if key != "" {
		l.perIP[key]++
	}
	return ""
}

func (l *limitListener) release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total--
	if key != "" {
		if l.perIP[key]--; l.perIP[key] <= 0 {
			delete(l.perIP, key)
		}
	}
}

// noteRefused logs refusals at most once a minute: a flood must not fill the log.
func (l *limitListener) noteRefused(reason string) {
	l.mu.Lock()
	l.refused[reason]++
	var counts map[string]int
	if time.Since(l.logged) >= time.Minute {
		counts, l.refused, l.logged = l.refused, map[string]int{}, time.Now()
	}
	l.mu.Unlock()
	if counts != nil && l.log != nil {
		l.log.Warn("Refused connections over a limit.", "event", "gateway.connections-refused",
			"over_global", counts["global"], "over_per_ip", counts["per-ip"])
	}
}

type limitedConn struct {
	net.Conn
	l    *limitListener
	key  string
	once sync.Once
}

func (c *limitedConn) Close() error {
	c.once.Do(func() { c.l.release(c.key) })
	return c.Conn.Close()
}
