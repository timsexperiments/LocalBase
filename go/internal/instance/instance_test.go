package instance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/root"
)

// fakeGateway is an HTTP server at the address a root's owner.json names, with this test's live
// PID. proven says whether it answers the identity check with that owner's id and root hash.
type fakeGateway struct {
	mu   sync.Mutex
	seen []*http.Request
	body map[string][]byte // what each call sent
}

func (g *fakeGateway) requests() []*http.Request {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]*http.Request(nil), g.seen...)
}

func ownedRoot(t *testing.T, proven bool, handler http.HandlerFunc) (*root.Root, *fakeGateway) {
	t.Helper()
	return ownedRootAt(t, "127.0.0.1:0", proven, handler)
}

// ownedRootAt is ownedRoot with the gateway listening at addr.
func ownedRootAt(t *testing.T, addr string, proven bool, handler http.HandlerFunc) (*root.Root, *fakeGateway) {
	t.Helper()
	r, err := root.Init(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeGateway{body: map[string][]byte{}}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/_localbase/instance" && req.Method == http.MethodGet {
			id, hash := "someone-else", "not-this-root"
			if proven && req.Header.Get("X-Localbase-Instance-Token") == "the-token" {
				id, hash = "the-instance", "the-root-hash"
			}
			json.NewEncoder(w).Encode(map[string]string{"instanceId": id, "rootHash": hash})
			return
		}
		b, _ := io.ReadAll(req.Body)
		g.mu.Lock()
		g.seen = append(g.seen, req)
		g.body[req.URL.Path] = b
		g.mu.Unlock()
		handler(w, req)
	}))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	p, _ := strconv.Atoi(port)
	lock := filepath.Join(r.Dir, "runtime", "gateway.lock")
	if err := os.MkdirAll(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _ := json.Marshal(map[string]any{"version": 2, "instanceId": "the-instance", "root": r.Dir, "rootHash": "the-root-hash",
		"pid": os.Getpid(), "startedAt": "2026-11-10T00:00:00Z", "host": host, "port": p, "instanceToken": "the-token"})
	if err := os.WriteFile(filepath.Join(lock, "owner.json"), owner, 0o600); err != nil {
		t.Fatal(err)
	}
	return r, g
}

// A gateway that proves it is the root's gets a client, and every call carries the instance token.
func TestDialAProvenGateway(t *testing.T) {
	r, g := ownedRoot(t, true, func(w http.ResponseWriter, req *http.Request) { w.Write([]byte(`{"ok":true}`)) })
	c, state, _ := Dial(r)
	if c == nil || state != "active" {
		t.Fatalf("Dial on a proven gateway: client %v, state %q", c, state)
	}
	a, err := c.Get(context.Background(), "/_localbase/placement-state", nil)
	if err != nil || a.Status != 200 {
		t.Fatalf("Get: %v %+v", err, a)
	}
	var out struct{ OK bool }
	if err := a.Decode(&out); err != nil || !out.OK {
		t.Errorf("Decode: %v %+v", err, out)
	}
	seen := g.requests()
	if len(seen) != 1 || seen[0].Header.Get("X-Localbase-Instance-Token") != "the-token" {
		t.Errorf("the call didn't carry the instance token: %v", seen)
	}
}

// A gateway that doesn't prove it is the root's gets no client and no call: only the identity
// check reaches it.
func TestDialRefusesAnUnprovenGateway(t *testing.T) {
	r, g := ownedRoot(t, false, func(w http.ResponseWriter, req *http.Request) {})
	c, state, detail := Dial(r)
	if c != nil || state == "active" || detail == "" {
		t.Fatalf("Dial on an unproven gateway: client %v, state %q, detail %q", c, state, detail)
	}
	if seen := g.requests(); len(seen) != 0 {
		t.Errorf("an unproven gateway was sent %d calls", len(seen))
	}
}

// With no owner record, there is no gateway to dial.
func TestDialWithNoGateway(t *testing.T) {
	r, err := root.Init(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	if c, state, _ := Dial(r); c != nil || state != "missing" {
		t.Errorf("Dial with no owner: client %v, state %q", c, state)
	}
}

// A query value is encoded, so one with &, a space or a control byte can't change the URL.
func TestAQueryValueCannotChangeTheURL(t *testing.T) {
	r, g := ownedRoot(t, true, func(w http.ResponseWriter, req *http.Request) { w.Write([]byte(`{}`)) })
	c, _, _ := Dial(r)
	id := "a b&model=other\x01"
	if _, err := c.Get(context.Background(), "/_localbase/placement-state", map[string][]string{"model": {id}}); err != nil {
		t.Fatal(err)
	}
	seen := g.requests()
	if len(seen) != 1 || seen[0].URL.Query().Get("model") != id || len(seen[0].URL.Query()["model"]) != 1 {
		t.Errorf("the query reached the gateway as %q", seen[0].URL.RawQuery)
	}
}

// Post sends its value as a JSON body.
func TestPostSendsJSON(t *testing.T) {
	r, g := ownedRoot(t, true, func(w http.ResponseWriter, req *http.Request) { w.WriteHeader(202); w.Write([]byte(`{}`)) })
	c, _, _ := Dial(r)
	a, err := c.Post(context.Background(), "/_localbase/model-management", map[string]string{"modelId": "m", "action": "cancel"})
	if err != nil || a.Status != 202 {
		t.Fatalf("Post: %v %+v", err, a)
	}
	seen := g.requests()
	if len(seen) != 1 || seen[0].Header.Get("Content-Type") != "application/json" ||
		!bytes.Equal(bytes.TrimSpace(g.body["/_localbase/model-management"]), []byte(`{"action":"cancel","modelId":"m"}`)) {
		t.Errorf("the body reached the gateway as %q", g.body["/_localbase/model-management"])
	}
}

// An answer larger than MaxAnswer is refused rather than read whole.
func TestAnAnswerPastTheLimitIsRefused(t *testing.T) {
	r, _ := ownedRoot(t, true, func(w http.ResponseWriter, req *http.Request) { w.Write(bytes.Repeat([]byte("x"), MaxAnswer+1)) })
	c, _, _ := Dial(r)
	if _, err := c.Get(context.Background(), "/big", nil); !errors.Is(err, ErrAnswerTooLarge) {
		t.Errorf("an answer past the limit: %v", err)
	}
	r2, _ := ownedRoot(t, true, func(w http.ResponseWriter, req *http.Request) { w.Write(bytes.Repeat([]byte("x"), MaxAnswer)) })
	c2, _, _ := Dial(r2)
	if a, err := c2.Get(context.Background(), "/big", nil); err != nil || len(a.Body) != MaxAnswer {
		t.Errorf("an answer at the limit: %v", err)
	}
}

// A call keeps to its context's deadline.
func TestACallKeepsToItsDeadline(t *testing.T) {
	r, _ := ownedRoot(t, true, func(w http.ResponseWriter, req *http.Request) {
		select {
		case <-req.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	c, _, _ := Dial(r)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Get(ctx, "/slow", nil); err == nil || time.Since(start) > 2*time.Second {
		t.Errorf("a slow answer past the deadline: %v after %v", err, time.Since(start))
	}
}

// A redirect isn't followed: the caller gets the 3xx as it came, and where it points never sees
// the instance token.
func TestARedirectIsNotFollowed(t *testing.T) {
	var elsewhere []string
	var mu sync.Mutex
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		elsewhere = append(elsewhere, req.URL.Path+" token="+req.Header.Get("X-Localbase-Instance-Token"))
		mu.Unlock()
	}))
	defer other.Close()
	r, _ := ownedRoot(t, true, func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, other.URL+"/caught", http.StatusTemporaryRedirect)
	})
	c, _, _ := Dial(r)
	a, err := c.Get(context.Background(), "/_localbase/placement-state", nil)
	if err != nil || a.Status != http.StatusTemporaryRedirect {
		t.Errorf("a redirect: %v %+v, want the 307 itself", err, a)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(elsewhere) > 0 {
		t.Errorf("the redirect was followed: %v", elsewhere)
	}
}

// nonLoopbackIPv4 is an address of this machine that isn't loopback, where a test can listen: Go
// never proxies a loopback host (127.0.0.2 included), so only such an address can show a proxy.
func nonLoopbackIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			if ln, err := net.Listen("tcp", n.IP.String()+":0"); err == nil {
				ln.Close()
				return n.IP.String()
			}
		}
	}
	return ""
}

// A call goes straight to the gateway even with HTTP_PROXY set. Go reads the proxy environment
// once per process, so the calls are made in a child run of this test with HTTP_PROXY set from the
// start; the "proxy" here forwards what it gets, and records it.
//
// Only the client's own calls are checked. The identity check before them is
// service.VerifiedOwnerState's, which has its own client (reported separately).
func TestACallDoesNotGoThroughAProxy(t *testing.T) {
	if os.Getenv("INSTANCE_PROXY_CHILD") == "1" {
		r, _ := ownedRootAt(t, os.Getenv("INSTANCE_PROXY_HOST")+":0", true, func(w http.ResponseWriter, req *http.Request) { w.Write([]byte(`{}`)) })
		c, state, detail := Dial(r)
		if c == nil {
			t.Fatalf("child: Dial %q %q", state, detail)
		}
		if _, err := c.Get(context.Background(), "/through-the-client", nil); err != nil {
			t.Fatalf("child: %v", err)
		}
		return
	}
	host := nonLoopbackIPv4()
	if host == "" {
		t.Skip("this machine has no non-loopback IPv4 address to listen on, and Go never proxies a loopback host")
	}
	var mu sync.Mutex
	var proxied []string
	direct := &http.Transport{Proxy: nil}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		proxied = append(proxied, req.URL.Path)
		mu.Unlock()
		out := req.Clone(req.Context())
		out.RequestURI = ""
		resp, err := direct.RoundTrip(out)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	defer proxy.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestACallDoesNotGoThroughAProxy$", "-test.count=1")
	cmd.Env = append(os.Environ(), "INSTANCE_PROXY_CHILD=1", "INSTANCE_PROXY_HOST="+host, "HTTP_PROXY="+proxy.URL, "http_proxy="+proxy.URL, "NO_PROXY=", "no_proxy=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the child run: %v\n%s", err, out)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Logf("through the proxy: %v (host %s)", proxied, host)
	for _, p := range proxied {
		if p == "/through-the-client" {
			t.Errorf("a call went through HTTP_PROXY: %v", proxied)
		}
	}
}
