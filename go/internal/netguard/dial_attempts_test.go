package netguard

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The dialer tries a name's addresses in goroutines of their own (Happy Eyeballs), and each attempt can
// meet a refusal. What one attempt keeps, another may read or replace at the same time.
func TestARefusalIsKeptSafelyFromEveryAttempt(t *testing.T) {
	var r refusal
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.set(&BlockedError{Host: "models.example.com", Class: Never})
			if i%2 == 0 {
				_ = r.get()
			}
		}()
	}
	wg.Wait()
	var blocked *BlockedError
	if !errors.As(r.get(), &blocked) {
		t.Errorf("kept %v", r.get())
	}
	var none refusal
	if none.get() != nil {
		t.Error("a dial nothing refused has a refusal")
	}
}

// dnsAnsweringBoth is a resolver whose every lookup answers with an IPv4 and an IPv6 address, so a dial
// has an attempt in each family.
func dnsAnsweringBoth(t *testing.T, v4 [4]byte, v6 [16]byte) *net.Resolver {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			q := buf[12:n]
			end := 0
			for q[end] != 0 {
				end += int(q[end]) + 1
			}
			question := q[:end+5]
			resp := []byte{buf[0], buf[1], 0x81, 0x80, 0, 1, 0, 0, 0, 0, 0, 0}
			resp = append(resp, question...)
			switch binary.BigEndian.Uint16(question[end+1:]) {
			case 1: // A
				resp[7] = 1
				resp = append(resp, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 1, 0, 4)
				resp = append(resp, v4[:]...)
			case 28: // AAAA
				resp[7] = 1
				resp = append(resp, 0xc0, 0x0c, 0, 28, 0, 1, 0, 0, 0, 1, 0, 16)
				resp = append(resp, v6[:]...)
			}
			pc.WriteTo(resp, from)
		}
	}()
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", pc.LocalAddr().String())
	}}
}

// A name whose addresses in both families are refused is refused, and nothing is reached.
func TestANameRefusedInBothFamiliesIsRefused(t *testing.T) {
	var reached atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	loopback6 := [16]byte{15: 1}
	p := Policy{Resolver: dnsAnsweringBoth(t, [4]byte{127, 0, 0, 1}, loopback6)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range 20 {
		conn, err := p.DialContext()(ctx, "tcp", net.JoinHostPort("models.example.com", port))
		if conn != nil {
			conn.Close()
		}
		if !IsBlocked(err) {
			t.Fatalf("%v", err)
		}
	}
	if reached.Load() != 0 {
		t.Errorf("reached %d times", reached.Load())
	}
}
