package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

type fakeConn struct {
	net.Conn
	remote net.Addr
	wrote  bytes.Buffer
	closed bool
}

func (c *fakeConn) RemoteAddr() net.Addr             { return c.remote }
func (c *fakeConn) Write(b []byte) (int, error)      { return c.wrote.Write(b) }
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }
func (c *fakeConn) Close() error                     { c.closed = true; return nil }

// fakeListener hands out the connections it is given, in order.
type fakeListener struct {
	net.Listener
	conns []*fakeConn
}

func (l *fakeListener) Accept() (net.Conn, error) {
	if len(l.conns) == 0 {
		return nil, errors.New("no more connections")
	}
	c := l.conns[0]
	l.conns = l.conns[1:]
	return c, nil
}

func conn(ip string) *fakeConn {
	return &fakeConn{remote: &net.TCPAddr{IP: net.ParseIP(ip), Port: 1000}}
}

// acceptAll drains the listener: admitted connections, and how many were refused.
func acceptAll(t *testing.T, limits ConnLimits, conns ...*fakeConn) ([]net.Conn, int) {
	t.Helper()
	ln := LimitConnections(&fakeListener{conns: conns}, limits, nil)
	return acceptFrom(ln, conns), refusedCount(conns)
}

func acceptFrom(ln net.Listener, _ []*fakeConn) []net.Conn {
	var out []net.Conn
	for {
		c, err := ln.Accept()
		if err != nil {
			return out
		}
		out = append(out, c)
	}
}

func refusedCount(conns []*fakeConn) int {
	n := 0
	for _, c := range conns {
		if c.closed {
			n++
		}
	}
	return n
}

func TestPerIPCap(t *testing.T) {
	a1, a2, a3, b1 := conn("192.0.2.1"), conn("192.0.2.1"), conn("192.0.2.1"), conn("192.0.2.2")
	admitted, refused := acceptAll(t, ConnLimits{PerIP: 2}, a1, a2, a3, b1)
	if len(admitted) != 3 || refused != 1 || !a3.closed {
		t.Fatalf("admitted %d, refused %d, third from one address closed: %v", len(admitted), refused, a3.closed)
	}
	if !strings.Contains(a3.wrote.String(), "503") {
		t.Errorf("refused with %q", a3.wrote.String())
	}
}

// A tunnel on this machine connects from loopback for everyone behind it, so only
// the global cap applies there.
func TestLoopbackIsNotCappedPerAddress(t *testing.T) {
	conns := []*fakeConn{conn("127.0.0.1"), conn("127.0.0.1"), conn("127.0.0.1"), conn("::1"), conn("::1")}
	if admitted, refused := acceptAll(t, ConnLimits{PerIP: 1}, conns...); len(admitted) != 5 || refused != 0 {
		t.Fatalf("admitted %d, refused %d", len(admitted), refused)
	}
	conns = []*fakeConn{conn("127.0.0.1"), conn("127.0.0.1"), conn("127.0.0.1")}
	if admitted, refused := acceptAll(t, ConnLimits{Global: 2, PerIP: 1}, conns...); len(admitted) != 2 || refused != 1 {
		t.Fatalf("global cap: admitted %d, refused %d", len(admitted), refused)
	}
}

func TestIPv6CountsByNetwork(t *testing.T) {
	a, b, c := conn("2001:db8:1:2::1"), conn("2001:db8:1:2::2"), conn("2001:db8:1:3::1")
	admitted, _ := acceptAll(t, ConnLimits{PerIP: 1}, a, b, c)
	if len(admitted) != 2 || !b.closed {
		t.Fatalf("admitted %d; second address in the same /64 closed: %v", len(admitted), b.closed)
	}
}

func TestClosingAConnectionFreesItsSlot(t *testing.T) {
	first, again, third := conn("192.0.2.1"), conn("192.0.2.1"), conn("192.0.2.1")
	l := &fakeListener{conns: []*fakeConn{first}}
	ln := LimitConnections(l, ConnLimits{Global: 1, PerIP: 1}, nil)
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	c.Close() // twice frees one slot
	l.conns = []*fakeConn{again, third}
	if _, err := ln.Accept(); err != nil {
		t.Fatal(err)
	}
	if again.closed {
		t.Error("the freed slot was not reused")
	}
	if _, err := ln.Accept(); err == nil || !third.closed {
		t.Errorf("a double close left an extra slot: %v, closed %v", err, third.closed)
	}
	if ll := ln.(*limitListener); ll.total != 1 || ll.perIP["192.0.2.1"] != 1 {
		t.Errorf("total %d, per address %v", ll.total, ll.perIP)
	}
}

func TestNoCapsLeaveTheListenerAlone(t *testing.T) {
	l := &fakeListener{}
	if got := LimitConnections(l, ConnLimits{}, nil); got != net.Listener(l) {
		t.Errorf("%T", got)
	}
}

// The refusal is a complete HTTP response, on a real connection.
func TestOverCapAnswerIsAResponse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	limited := LimitConnections(ln, ConnLimits{Global: 1}, nil)
	defer limited.Close()
	go func() {
		for {
			if _, err := limited.Accept(); err != nil {
				return
			}
		}
	}()
	first, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	time.Sleep(50 * time.Millisecond) // accepted
	second, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	second.SetDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(second), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != 503 || out.Error.Code != "too_busy" || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("%d %+v %v", resp.StatusCode, out, err)
	}
}

func TestCapForFileLimit(t *testing.T) {
	for _, c := range []struct {
		global int
		files  uint64
		want   int
	}{{4096, 1 << 20, 4096}, {4096, 1024, 896}, {4096, 256, 128}, {4096, 100, 64}, {0, 256, 0}, {4096, 0, 4096}} {
		if got := CapForFileLimit(c.global, c.files); got != c.want {
			t.Errorf("CapForFileLimit(%d, %d) = %d, want %d", c.global, c.files, got, c.want)
		}
	}
}
