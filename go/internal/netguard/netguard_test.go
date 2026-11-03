package netguard

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		ip   string
		want Class
	}{
		// public
		{"8.8.8.8", Public}, {"1.1.1.1", Public}, {"172.15.0.1", Public}, {"172.32.0.1", Public}, {"192.169.0.1", Public},
		{"100.63.255.255", Public}, {"100.128.0.1", Public}, {"2606:4700::1111", Public},
		{"::ffff:8.8.8.8", Public}, {"64:ff9b::808:808", Public},
		// private: this machine and its networks
		{"127.0.0.1", Private}, {"127.1.2.3", Private}, {"10.0.0.1", Private}, {"172.16.0.1", Private}, {"172.31.255.255", Private},
		{"192.168.1.1", Private}, {"100.64.0.1", Private}, {"::1", Private}, {"fc00::1", Private}, {"fd12:3456::1", Private},
		{"::ffff:127.0.0.1", Private}, {"::ffff:10.0.0.1", Private}, {"64:ff9b::7f00:1", Private}, {"64:ff9b::a00:1", Private},
		// never: link-local, metadata, not real destinations
		{"169.254.169.254", Never}, {"169.254.0.1", Never}, {"::ffff:169.254.169.254", Never}, {"64:ff9b::a9fe:a9fe", Never},
		{"fd00:ec2::254", Never}, {"fe80::1", Never}, {"100.100.100.200", Never}, {"168.63.129.16", Never}, {"192.0.0.192", Never},
		{"0.0.0.0", Never}, {"0.1.2.3", Never}, {"::", Never}, {"224.0.0.1", Never}, {"255.255.255.255", Never}, {"240.0.0.1", Never},
		{"ff02::1", Never}, {"192.0.2.1", Never}, {"198.51.100.1", Never}, {"203.0.113.1", Never}, {"198.18.0.1", Never},
		{"2001:db8::1", Never}, {"2002:7f00:1::", Never}, {"2001:0:7f00:1::", Never}, {"::127.0.0.1", Never}, {"::10.0.0.1", Never},
	} {
		if got := Classify(netip.MustParseAddr(c.ip)); got != c.want {
			t.Errorf("%s: class %d, want %d", c.ip, got, c.want)
		}
	}
	if Classify(netip.MustParseAddr("fe80::1%en0")) != Never {
		t.Error("a zoned address")
	}
	if Classify(netip.Addr{}) != Never {
		t.Error("the zero address")
	}
}

// Every way of writing an address is judged by the address it means.
func TestCheckURL(t *testing.T) {
	blocked := []string{
		"http://127.0.0.1/", "http://127.0.0.1:8080/", "http://127.1/", "http://127.0.1/", "http://0x7f.0.0.1/", "http://0x7f000001/", "http://2130706433/",
		"http://0177.0.0.1/", "http://017700000001/", "http://0/", "http://0.0.0.0/", "http://[::1]/", "http://[::]/", "http://[::ffff:127.0.0.1]/",
		"http://[::ffff:7f00:1]/", "http://[0:0:0:0:0:ffff:7f00:1]/", "http://[64:ff9b::7f00:1]/", "http://[fe80::1%25en0]/",
		"http://10.0.0.1/", "http://012.0.0.1/", "http://0xa000001/", "http://167772161/", "http://172.16.0.1/", "http://192.168.1.1/", "http://192.168.1.1./",
		"http://100.64.0.1/", "http://[fc00::1]/", "http://169.254.169.254/latest/meta-data/", "http://2852039166/", "http://0xa9fea9fe/",
		"http://[fd00:ec2::254]/", "http://localhost/", "http://LOCALHOST:8080/", "http://localhost./", "http://foo.localhost/", "http://printer.local/",
		"http://metadata.google.internal/", "http://router.lan/", "http://nas.home.arpa/", "https://169.254.169.254/",
	}
	for _, raw := range blocked {
		u, err := url.Parse(raw)
		if err != nil {
			t.Errorf("%s: %v", raw, err)
			continue
		}
		var b *BlockedError
		if err := (Policy{}).CheckURL(u); !errors.As(err, &b) {
			t.Errorf("%s: %v, want a refusal", raw, err)
		}
	}
	for _, raw := range []string{"https://huggingface.co/", "https://example.com:8443/x", "https://8.8.8.8/", "http://[2606:4700::1111]/",
		"https://127.0.0.1.example.com/", "https://localhost.example.com/", "https://0x7f.example.com/", "https://1.2.3.4.5/"} {
		u, _ := url.Parse(raw)
		if err := (Policy{}).CheckURL(u); err != nil {
			t.Errorf("%s: %v, want it to pass", raw, err)
		}
	}
}

// A metadata address stays refused even with the opt-in; a private one does not.
func TestOptIn(t *testing.T) {
	check := func(p Policy, raw string) error {
		u, _ := url.Parse(raw)
		return p.CheckURL(u)
	}
	in := Policy{AllowPrivate: true}
	for _, raw := range []string{"http://10.0.0.1/", "http://127.0.0.1/", "http://localhost/", "http://nas.lan/", "http://[fc00::1]/"} {
		if err := check(in, raw); err != nil {
			t.Errorf("%s with the opt-in: %v", raw, err)
		}
	}
	for _, raw := range []string{"http://169.254.169.254/", "http://[fd00:ec2::254]/", "http://[fe80::1]/", "http://168.63.129.16/", "http://0.0.0.0/"} {
		if err := check(in, raw); err == nil {
			t.Errorf("%s was allowed by the opt-in", raw)
		}
	}
	named := Policy{AllowHosts: []string{"mirror.lan", "10.1.1.1"}}
	if err := check(named, "http://MIRROR.lan:8080/"); err != nil {
		t.Errorf("a named host: %v", err)
	}
	if err := check(named, "http://10.1.1.1/"); err != nil {
		t.Errorf("a named address: %v", err)
	}
	if err := check(named, "http://other.lan/"); err == nil {
		t.Error("a host that was not named")
	}
	var b *BlockedError
	if err := check(Policy{}, "http://10.0.0.1/"); !errors.As(err, &b) || b.Class != Private {
		t.Errorf("a private address: %v", err)
	}
	if err := check(Policy{}, "http://169.254.169.254/"); !errors.As(err, &b) || b.Class != Never {
		t.Errorf("a metadata address: %v", err)
	}
}

// dnsAnswering is a resolver whose every lookup answers with one address, whatever
// the name: what a name that rebinds to a private address looks like when it connects.
func dnsAnswering(t *testing.T, ip [4]byte) *net.Resolver {
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
			qtype := binary.BigEndian.Uint16(question[end+1:])
			resp := []byte{buf[0], buf[1], 0x81, 0x80, 0, 1, 0, 0, 0, 0, 0, 0}
			resp = append(resp, question...)
			if qtype == 1 { // A
				resp[7] = 1
				resp = append(resp, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 1, 0, 4, ip[0], ip[1], ip[2], ip[3])
			}
			pc.WriteTo(resp, from)
		}
	}()
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", pc.LocalAddr().String())
	}}
}

func TestConnectTimeCheck(t *testing.T) {
	var reached atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1); io.WriteString(w, "ok") }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	get := func(p Policy, raw string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		// Direct: this test judges the dial, not a proxy from the environment.
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = nil
		resp, err := p.Transport(tr).RoundTrip(req)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	t.Run("an address", func(t *testing.T) {
		reached.Store(0)
		if err := get(Policy{}, srv.URL); !IsBlocked(err) || reached.Load() != 0 {
			t.Errorf("%v, reached %d", err, reached.Load())
		}
	})
	t.Run("a name that looks public and resolves to loopback", func(t *testing.T) {
		reached.Store(0)
		p := Policy{Resolver: dnsAnswering(t, [4]byte{127, 0, 0, 1})}
		if err := get(p, "http://models.example.com:"+port+"/"); !IsBlocked(err) || reached.Load() != 0 {
			t.Errorf("%v, reached %d", err, reached.Load())
		}
		if err := get(p, "http://models.example.com:"+port+"/"); !IsBlocked(err) {
			t.Errorf("second connection: %v", err)
		}
	})
	t.Run("a name that resolves to a metadata address", func(t *testing.T) {
		p := Policy{AllowPrivate: true, Resolver: dnsAnswering(t, [4]byte{169, 254, 169, 254})}
		if err := get(p, "http://models.example.com/"); !IsBlocked(err) {
			t.Errorf("%v", err)
		}
	})
	t.Run("the opt-in reaches it", func(t *testing.T) {
		reached.Store(0)
		p := Policy{AllowPrivate: true, Resolver: dnsAnswering(t, [4]byte{127, 0, 0, 1})}
		if err := get(p, "http://models.example.com:"+port+"/"); err != nil || reached.Load() != 1 {
			t.Errorf("%v, reached %d", err, reached.Load())
		}
	})
	t.Run("a host the person named reaches it", func(t *testing.T) {
		reached.Store(0)
		host, _, _ := net.SplitHostPort(srv.Listener.Addr().String())
		if err := get(Policy{AllowHosts: []string{host}}, srv.URL); err != nil || reached.Load() != 1 {
			t.Errorf("%v, reached %d", err, reached.Load())
		}
	})
}

// A redirect is a new destination, judged like the first one.
func TestRedirectsAreJudged(t *testing.T) {
	var private atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { private.Add(1) }))
	defer target.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/secret", http.StatusFound)
	}))
	defer first.Close()
	firstHost, _, _ := net.SplitHostPort(first.Listener.Addr().String())
	// Named by host and port would be needed to tell these apart, so name a second
	// loopback address for the first server's URL.
	firstURL := strings.Replace(first.URL, firstHost, "localhost", 1)
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	client := &http.Client{Transport: Policy{AllowHosts: []string{"localhost"}}.Transport(tr)}
	resp, err := client.Get(firstURL)
	if err == nil {
		resp.Body.Close()
	}
	if !IsBlocked(err) || private.Load() != 0 {
		t.Fatalf("%v, reached %d", err, private.Load())
	}
}

// Connecting to a configured proxy is allowed, but what is behind it is still judged.
func TestProxyIsNotTheDestination(t *testing.T) {
	var through atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { through.Add(1); io.WriteString(w, "ok") }))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	proxyURL, _ := url.Parse(proxy.URL)
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyURL(proxyURL)
	rt := Policy{}.Transport(tr)
	req, _ := http.NewRequest(http.MethodGet, "http://169.254.169.254/latest/meta-data/", nil)
	if _, err := rt.RoundTrip(req); !IsBlocked(err) || through.Load() != 0 {
		t.Errorf("metadata through a proxy: %v, proxy saw %d", err, through.Load())
	}
	req, _ = http.NewRequest(http.MethodGet, "http://10.0.0.1/", nil)
	if _, err := rt.RoundTrip(req); !IsBlocked(err) || through.Load() != 0 {
		t.Errorf("private through a proxy: %v, proxy saw %d", err, through.Load())
	}
	req, _ = http.NewRequest(http.MethodGet, "http://models.example.com/", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil || through.Load() != 1 {
		t.Fatalf("a public name through a private proxy: %v, proxy saw %d", err, through.Load())
	}
	resp.Body.Close()
}
