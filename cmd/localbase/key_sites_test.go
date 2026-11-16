package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
)

// keyRecorder records each request's route and the key headers it carried.
type keyRecorder struct {
	mu  sync.Mutex
	got []string
}

func (k *keyRecorder) add(r *http.Request) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.got = append(k.got, r.Method+" "+r.URL.Path+" authorization="+r.Header.Get("Authorization")+" x-api-key="+r.Header.Get("X-Api-Key"))
}

func (k *keyRecorder) all() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.got...)
}

// smoke sends its temporary key, as Authorization and X-Api-Key, to the proven gateway alone. Here
// the gateway proves itself and answers /health, then redirects the first keyed request to a server
// that would answer it. No redirect is followed, so that server sees no request at all, neither key
// header goes anywhere else, and smoke reports the 3xx as a failed step, not a pass.
func TestSmokeSendsItsKeyNowhereButTheGateway(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	dir := t.TempDir() + "/root"
	if code, out := runApp(t, "--root", dir, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	e, err := readEnv(&cli.Ctx{Global: cli.Global{Root: dir}}, true)
	if err != nil {
		t.Fatal(err)
	}
	var elsewhere, atGateway keyRecorder
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.add(r)
		w.Write([]byte(`{"data":[]}`))
	}))
	defer other.Close()
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_localbase/instance":
			json.NewEncoder(w).Encode(map[string]string{"instanceId": "id", "rootHash": "hash"})
		case "/health":
			w.Write([]byte(`{"status":"ok","version":"x","modalities":{"llm":{"configured":true,"state":"ready"}}}`))
		default:
			atGateway.add(r)
			http.Redirect(w, r, other.URL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
		}
	}))
	defer gw.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(gw.URL, "http://"))
	writeOwner(t, e, host, atoi(t, port), "id", "hash")
	e.Close() // smoke opens the root itself

	_, out := runApp(t, "--root", dir, "--json", "smoke", "--timeout", "5")
	if !strings.Contains(out, "Smoke check failed") || !strings.Contains(out, "HTTP 307") || strings.Contains(out, "Smoke check passed") {
		t.Errorf("smoke through a redirect should fail with the 307:\n%s", out)
	}
	keyed := atGateway.all()
	if len(keyed) == 0 || !strings.Contains(keyed[0], "authorization=Bearer ") || strings.HasSuffix(keyed[0], "x-api-key=") {
		t.Errorf("the gateway should get the keyed request, with both headers: %v", keyed)
	}
	if got := elsewhere.all(); len(got) > 0 {
		t.Errorf("a redirect was followed; the other server got:\n%s", strings.Join(got, "\n"))
	}
}

// upgrade asks the gateway how busy it is with a temporary key. A redirect is not followed: the
// question fails with the 3xx, and the place it points sees no request and no key.
func TestUpgradeAsksHowBusyOnlyOfTheGateway(t *testing.T) {
	var elsewhere keyRecorder
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.add(r)
		w.Write([]byte(`{"host":{"activity":{"busy":false}}}`))
	}))
	defer other.Close()
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	defer gw.Close()
	if busy, detail, err := gatewayActivity(gw.URL, "the-key"); err == nil || !strings.Contains(err.Error(), "HTTP 307") {
		t.Errorf("gatewayActivity through a redirect: busy %v, %q, %v; want an HTTP 307 error", busy, detail, err)
	}
	if got := elsewhere.all(); len(got) > 0 {
		t.Errorf("a redirect was followed; the other server got:\n%s", strings.Join(got, "\n"))
	}
}
