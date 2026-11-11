package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
)

// A live PID in owner.json isn't proof that the gateway it names is answering at that address: the
// PID can be reused, and something else can listen there. The CLI asks for proof (instance id and
// root hash) before it sends the instance token on any other route or writes through it
// (review 01-M7). Here an impostor answers with someone else's identity.
func TestTheCLITrustsOnlyAProvenGateway(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	dir := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", dir, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	var mu sync.Mutex
	var asked []string
	impostor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_localbase/instance" && r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]string{"instanceId": "someone-else", "rootHash": "not-this-root"})
			return
		}
		mu.Lock()
		asked = append(asked, r.Method+" "+r.URL.Path+" token="+r.Header.Get("X-Localbase-Instance-Token"))
		mu.Unlock()
		w.Write([]byte(`{"modelId":"x","defaults":["x"]}`))
	}))
	defer impostor.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(impostor.URL, "http://"))
	e, err := readEnv(&cli.Ctx{Global: cli.Global{Root: dir}}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	lock := filepath.Join(e.Root.Dir, "runtime", "gateway.lock")
	if err := os.MkdirAll(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _ := json.Marshal(map[string]any{"version": 2, "instanceId": "the-real-one", "root": e.Root.Dir, "rootHash": "this-root",
		"pid": os.Getpid(), "startedAt": "2026-11-06T00:00:00Z", "host": host, "port": atoi(t, port), "instanceToken": "the-token"})
	if err := os.WriteFile(filepath.Join(lock, "owner.json"), owner, 0o600); err != nil {
		t.Fatal(err)
	}

	if d := gatewayDefaultModel(e); d != nil {
		t.Errorf("default model from an unproven gateway: %+v", d)
	}
	if l := gatewayPlacement(e, "qwen3.5-4b-q4_k_m"); l != nil {
		t.Errorf("placement from an unproven gateway: %+v", l)
	}
	if o := reportLogOutages(&cli.Ctx{Stdout: new(strings.Builder), Stderr: new(strings.Builder)}, e); o != nil {
		t.Errorf("log outages from an unproven gateway: %+v", o)
	}
	wrote := false
	if _, err := writeThroughGateway(e, "/_localbase/usage", map[string]any{"action": "clear"}, func() (map[string]any, error) {
		wrote = true
		return map[string]any{}, nil
	}); err == nil || wrote {
		t.Errorf("a write with an unproven gateway: err %v, wrote the database directly %v", err, wrote)
	}
	if code, out := runApp(t, "--root", dir, "models", "cancel", "qwen3.5-4b-q4_k_m"); code == 0 || !strings.Contains(out, "Cannot tell whether a gateway is running on this root") {
		t.Errorf("models cancel with an unproven gateway: exit %d, %s", code, out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) > 0 {
		t.Errorf("the impostor got requests beyond the identity check:\n%s", strings.Join(asked, "\n"))
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// writeOwner records a gateway owner for e's root, alive (this process) at host:port.
func writeOwner(t *testing.T, e *env, host string, port int, instanceID, rootHash string) {
	t.Helper()
	lock := filepath.Join(e.Root.Dir, "runtime", "gateway.lock")
	if err := os.MkdirAll(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _ := json.Marshal(map[string]any{"version": 2, "instanceId": instanceID, "root": e.Root.Dir, "rootHash": rootHash,
		"pid": os.Getpid(), "startedAt": "2026-11-06T00:00:00Z", "host": host, "port": port, "instanceToken": "the-token"})
	if err := os.WriteFile(filepath.Join(lock, "owner.json"), owner, 0o600); err != nil {
		t.Fatal(err)
	}
}

// An owner record whose host isn't one, and a model id that isn't URL-safe: requests are built with
// their errors checked and the id escaped (review 01-M4). Before, NewRequest's error was dropped,
// so req was nil and the next line panicked, and "a&b" reached the gateway as "a".
func TestRequestsToTheGatewayAreBuiltSafely(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	dir := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", dir, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	e, err := readEnv(&cli.Ctx{Global: cli.Global{Root: dir}}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	writeOwner(t, e, "127.0.0.1\x7f", 1, "id", "hash") // not a host
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Errorf("a bad host in owner.json panicked: %v", p)
			}
		}()
		_ = gatewayDefaultModel(e)
		_ = gatewayPlacement(e, "m")
		_ = reportLogOutages(&cli.Ctx{}, e)
	}()

	var got string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_localbase/instance":
			json.NewEncoder(w).Encode(map[string]string{"instanceId": "id", "rootHash": "hash"})
		case "/_localbase/placement-state":
			got = r.URL.Query().Get("model")
			w.Write([]byte(`{}`))
		}
	}))
	defer gw.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(gw.URL, "http://"))
	writeOwner(t, e, host, atoi(t, port), "id", "hash")
	const id = "a&b c"
	if gatewayPlacement(e, id) == nil || got != id {
		t.Errorf("the gateway was asked about %q, want %q", got, id)
	}
}
