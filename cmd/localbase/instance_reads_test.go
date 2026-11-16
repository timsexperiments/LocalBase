package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
)

// readSitesEnv is a configured root, open, for the read sites' tests.
func readSitesEnv(t *testing.T) *env {
	t.Helper()
	t.Setenv("LOCALBASE_ROOT", "")
	dir := t.TempDir() + "/root"
	if code, out := runApp(t, "--root", dir, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	e, err := readEnv(&cli.Ctx{Global: cli.Global{Root: dir}}, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}

// A proven gateway is asked on each read site's own route, with the instance token, and what it
// answers is what the site returns.
func TestTheReadSitesAskTheProvenGateway(t *testing.T) {
	e := readSitesEnv(t)
	var mu sync.Mutex
	var asked []string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.Path+"?"+r.URL.RawQuery+" token="+r.Header.Get("X-Localbase-Instance-Token"))
		mu.Unlock()
		switch r.URL.Path {
		case "/_localbase/instance":
			json.NewEncoder(w).Encode(map[string]any{"instanceId": "id", "rootHash": "hash",
				"logOutages": []map[string]any{{"log": "requests", "reason": "the disk is full"}}})
		case "/_localbase/default-model":
			w.Write([]byte(`{"modelId":"the-default","defaults":["the-default"]}`))
		case "/_localbase/placement-state":
			w.Write([]byte(`{"loaded":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer gw.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(gw.URL, "http://"))
	writeOwner(t, e, host, atoi(t, port), "id", "hash")

	if d := gatewayDefaultModel(e); d == nil || d.ModelID != "the-default" {
		t.Errorf("gatewayDefaultModel: %+v", d)
	}
	if l := gatewayPlacement(e, "m"); l == nil || !l.Loaded {
		t.Errorf("gatewayPlacement: %+v", l)
	}
	var out bytes.Buffer
	if o := reportLogOutages(&cli.Ctx{Stdout: &out, Stderr: &out}, e); len(o) != 1 || o[0].Log != "requests" {
		t.Errorf("reportLogOutages: %+v", o)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"/_localbase/default-model?", "/_localbase/placement-state?model=m", "/_localbase/instance?"}
	for _, w := range want {
		found := false
		for _, a := range asked {
			found = found || a == w+" token=the-token"
		}
		if !found {
			t.Errorf("no %q with the token among:\n%s", w, strings.Join(asked, "\n"))
		}
	}
	for _, a := range asked {
		if !strings.HasSuffix(a, " token=the-token") {
			t.Errorf("a request without the token: %s", a)
		}
	}
}

// The read sites follow no redirect. The proven gateway answers its identity check, then sends
// each read elsewhere, to a server that answers as the gateway would: following it would hand that
// server the token and return its answer. reportLogOutages reads the identity route itself, so
// once it runs, the second call there (its read, after its own identity check) is the one sent
// elsewhere.
func TestTheReadSitesFollowNoRedirect(t *testing.T) {
	e := readSitesEnv(t)
	var mu sync.Mutex
	var elsewhere []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		elsewhere = append(elsewhere, r.URL.Path+" token="+r.Header.Get("X-Localbase-Instance-Token"))
		mu.Unlock()
		switch r.URL.Path {
		case "/_localbase/instance":
			json.NewEncoder(w).Encode(map[string]any{"instanceId": "id", "rootHash": "hash",
				"logOutages": []map[string]any{{"log": "requests", "reason": "elsewhere"}}})
		case "/_localbase/default-model":
			w.Write([]byte(`{"modelId":"elsewhere","defaults":["elsewhere"]}`))
		case "/_localbase/placement-state":
			w.Write([]byte(`{"loaded":true}`))
		}
	}))
	defer other.Close()
	logsRead, instanceCalls := false, 0
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_localbase/instance" {
			mu.Lock()
			if logsRead {
				instanceCalls++
			}
			check := !logsRead || instanceCalls == 1
			mu.Unlock()
			if check {
				json.NewEncoder(w).Encode(map[string]string{"instanceId": "id", "rootHash": "hash"})
				return
			}
		}
		http.Redirect(w, r, other.URL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	defer gw.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(gw.URL, "http://"))
	writeOwner(t, e, host, atoi(t, port), "id", "hash")

	if d := gatewayDefaultModel(e); d != nil {
		t.Errorf("gatewayDefaultModel through a redirect: %+v", d)
	}
	if l := gatewayPlacement(e, "m"); l != nil {
		t.Errorf("gatewayPlacement through a redirect: %+v", l)
	}
	mu.Lock()
	logsRead = true
	mu.Unlock()
	var out bytes.Buffer
	if o := reportLogOutages(&cli.Ctx{Stdout: &out, Stderr: &out}, e); o != nil {
		t.Errorf("reportLogOutages through a redirect: %+v", o)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(elsewhere) > 0 {
		t.Errorf("a redirect was followed:\n%s", strings.Join(elsewhere, "\n"))
	}
}
