package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
)

// upgradeImpostor is a root whose owner.json names a live PID (this test's), a service, and an
// address where something answers with another root's identity: what the upgrade would take for a
// managed gateway if a live PID were proof. asked records every request that carries a credential.
func upgradeImpostor(t *testing.T) (e *env, asked func() []string) {
	t.Helper()
	t.Setenv("LOCALBASE_ROOT", "")
	dir := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", dir, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	var mu sync.Mutex
	var got []string
	impostor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_localbase/instance" && r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]string{"instanceId": "someone-else", "rootHash": "not-this-root"})
			return
		}
		if a := r.Header.Get("Authorization"); a != "" {
			mu.Lock()
			got = append(got, r.Method+" "+r.URL.Path+" "+a)
			mu.Unlock()
		}
		w.Write([]byte(`{"busy":false}`))
	}))
	t.Cleanup(impostor.Close)
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(impostor.URL, "http://"))
	e, err := readEnv(&cli.Ctx{Global: cli.Global{Root: dir}}, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	lock := filepath.Join(e.Root.Dir, "runtime", "gateway.lock")
	if err := os.MkdirAll(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _ := json.Marshal(map[string]any{"version": 2, "instanceId": "the-real-one", "root": e.Root.Dir, "rootHash": "this-root",
		"pid": os.Getpid(), "startedAt": "2026-11-10T00:00:00Z", "host": host, "port": atoi(t, port), "instanceToken": "the-token",
		"serviceId": "com.localbase.gateway.test"})
	if err := os.WriteFile(filepath.Join(lock, "owner.json"), owner, 0o600); err != nil {
		t.Fatal(err)
	}
	return e, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), got...) }
}

// upgrade takes a gateway for its own, and asks it how busy it is with a temporary key, only once
// the gateway has proven it is this root's. A live PID and a listener at the recorded address that
// answers for another root get no key, and the upgrade refuses, changing nothing.
func TestUpgradeSendsItsKeyOnlyToAProvenGateway(t *testing.T) {
	e, asked := upgradeImpostor(t)
	facts, _ := gatherGatewayFacts(e, "/nowhere/local-base")
	if facts.lockState == "active" || facts.ownerManaged {
		t.Errorf("an unproven owner was taken for a running, managed gateway: %+v", facts)
	}
	if _, err := classifyGateway(facts); err == nil || !strings.Contains(err.Error(), "Cannot tell whether a gateway is running on this root") {
		t.Errorf("classifyGateway with an unproven owner: %v", err)
	}
	// The wait, as runUpgradeInstall runs it: a base only from a proven gateway, then the key.
	var stderr bytes.Buffer
	c := &cli.Ctx{Stdout: &stderr, Stderr: &stderr}
	base, err := idleWaitBase(e)
	if err == nil {
		waitGatewayIdle(c, e, base, 50*time.Millisecond, func(time.Duration) {})
	}
	if err == nil || !strings.Contains(err.Error(), "Cannot tell whether a gateway is running on this root") {
		t.Errorf("idleWaitBase with an unproven owner: %q, %v", base, err)
	}
	if got := asked(); len(got) > 0 {
		t.Errorf("the impostor was sent a credential:\n%s", strings.Join(got, "\n"))
	}
}

// With no owner record at all, the wait is refused rather than reading a nil owner.
func TestUpgradeWaitsForNoGatewayThatIsNotThere(t *testing.T) {
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
	if base, err := idleWaitBase(e); err == nil || base != "" {
		t.Errorf("idleWaitBase with no owner: %q, %v", base, err)
	}
}
