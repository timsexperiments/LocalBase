package service

import (
	"encoding/json"
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

	"github.com/timsexperiments/LocalBase/go/internal/root"
)

// recorder is an HTTP server that records the path and instance token of every request it gets.
type recorder struct {
	mu   sync.Mutex
	seen []string
}

func (rec *recorder) add(r *http.Request) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.seen = append(rec.seen, r.URL.Path+" token="+r.Header.Get("X-Localbase-Instance-Token"))
}

func (rec *recorder) got() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]string(nil), rec.seen...)
}

// answerAsTheGateway answers the identity check, /health and /health/ready as the gateway that
// owns the root in ownedRootAt would.
func answerAsTheGateway(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/_localbase/instance":
		if r.Header.Get("X-Localbase-Instance-Token") != "the-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"instanceId": "the-instance", "rootHash": "the-root-hash"})
	case "/health":
		w.Write([]byte(`{"status":"ok"}`))
	case "/health/ready":
		w.Write([]byte(`{"status":"ready"}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// ownedRootAt is a root whose owner.json names this test's live PID and a gateway listening at
// addr, served by handler.
func ownedRootAt(t *testing.T, addr string, handler http.HandlerFunc) (*root.Root, *root.Owner) {
	t.Helper()
	r, err := root.Init(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(handler)
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
	state, o, detail := r.OwnerState()
	if state != "active" || o == nil {
		t.Fatalf("the owner record: %q %q", state, detail)
	}
	return r, o
}

// Nothing that talks to the owner follows a redirect. Here the recorded address redirects every
// route to a server that answers exactly as the real gateway would, so following it would prove
// the owner, read its health and its readiness, and hand that server the instance token.
func TestTheOwnerProbesFollowNoRedirect(t *testing.T) {
	var elsewhere recorder
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.add(r)
		answerAsTheGateway(w, r)
	}))
	defer other.Close()
	r, o := ownedRootAt(t, "127.0.0.1:0", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, other.URL+req.URL.Path, http.StatusTemporaryRedirect)
	})
	if state, _, _ := VerifiedOwnerState(r); state != "unknown" {
		t.Errorf("VerifiedOwnerState through a redirect: %q, want unknown", state)
	}
	if h := readHealth(o); h != nil {
		t.Errorf("readHealth through a redirect: %s, want nothing", h)
	}
	if answersReady(o.HealthURL()) {
		t.Error("answersReady through a redirect: ready, want not")
	}
	if got := elsewhere.got(); len(got) > 0 {
		t.Errorf("a redirect was followed:\n%s", strings.Join(got, "\n"))
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

// Nothing that talks to the owner goes through HTTP_PROXY. Go reads the proxy environment once
// per process, so the probes run in a child run of this test with HTTP_PROXY set from the start,
// against a gateway on a non-loopback address of this machine. The "proxy" forwards what it gets,
// so a proxied probe would still succeed; only the proxy's record shows it.
func TestTheOwnerProbesDoNotGoThroughAProxy(t *testing.T) {
	if os.Getenv("SERVICE_PROXY_CHILD") == "1" {
		r, o := ownedRootAt(t, os.Getenv("SERVICE_PROXY_HOST")+":0", answerAsTheGateway)
		if state, _, detail := VerifiedOwnerState(r); state != "active" {
			t.Errorf("child: VerifiedOwnerState %q %q", state, detail)
		}
		if readHealth(o) == nil {
			t.Error("child: readHealth read nothing")
		}
		if !answersReady(o.HealthURL()) {
			t.Error("child: answersReady: not ready")
		}
		return
	}
	host := nonLoopbackIPv4()
	if host == "" {
		t.Skip("this machine has no non-loopback IPv4 address to listen on, and Go never proxies a loopback host")
	}
	var proxied recorder
	direct := &http.Transport{Proxy: nil}
	defer direct.CloseIdleConnections()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		proxied.add(req)
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
	cmd := exec.Command(os.Args[0], "-test.run=^TestTheOwnerProbesDoNotGoThroughAProxy$", "-test.count=1")
	cmd.Env = append(os.Environ(), "SERVICE_PROXY_CHILD=1", "SERVICE_PROXY_HOST="+host,
		"HTTP_PROXY="+proxy.URL, "http_proxy="+proxy.URL, "NO_PROXY=", "no_proxy=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the child run: %v\n%s", err, out)
	}
	t.Logf("through the proxy: %v (host %s)", proxied.got(), host)
	if got := proxied.got(); len(got) > 0 {
		t.Errorf("a probe went through HTTP_PROXY:\n%s", strings.Join(got, "\n"))
	}
}
