package keys

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// The tests that hold a change just before its write, through beforeAdminWrite, run against a
// Handler alone: the lock they test is the Handler's. gateway/key_authority_test.go and
// gateway/access_authority_test.go keep a whole gateway for what goes over HTTP.

// keysHarness serves a Handler's three routes, with a real root and store.
type keysHarness struct {
	t    *testing.T
	ts   *httptest.Server
	h    *Handler
	st   *store.Store
	root *root.Root
}

// newKeysHarness: a request's caller is its bearer key, resolved as the gateway resolves one; a
// request without one is the host, as the gateway's own calls are.
func newKeysHarness(t *testing.T) *keysHarness {
	t.Helper()
	r, err := root.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	resolver := &auth.Resolver{Store: st, Mode: "either"}
	host := &auth.Principal{Kind: "environment", Permissions: auth.Permissions}
	h := NewHandler(Options{
		Store:   st,
		RootDir: r.Dir,
		Browser: func() *auth.Browser { return nil },
		Caller: func(req *http.Request) *auth.Principal {
			if req.Header.Get("Authorization") == "" {
				return host
			}
			p, _ := resolver.ResolveAPI(req.Context(), req)
			return p
		},
	})
	mux := http.NewServeMux()
	mux.HandleFunc(gatewaytest.KeysRoute, h.APIKeys)
	mux.HandleFunc(gatewaytest.AccessRoute, h.AccessManagement)
	mux.HandleFunc("/_localbase/hf-token", h.HFToken)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &keysHarness{t: t, ts: ts, h: h, st: st, root: r}
}

// post sends body to path with secret as its bearer key, or as the host when secret is empty, and
// returns the status and the body.
func (k *keysHarness) post(secret, path, body string) (int, string) {
	k.t.Helper()
	req, err := http.NewRequest(http.MethodPost, k.ts.URL+path, strings.NewReader(body))
	if err != nil {
		k.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		k.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// key makes a key directly in the store and returns its record and secret.
func (k *keysHarness) key(name string, scopes ...string) (*store.APIKey, string) {
	k.t.Helper()
	rec, raw, err := k.st.CreateAPIKeyExpiring(context.Background(), name, scopes, 0)
	if err != nil {
		k.t.Fatal(err)
	}
	return rec, raw
}

// holdBeforeWrite makes each change wait just before its write until both of two have arrived, or
// a moment has passed: with the lock held, the other can't arrive.
func (k *keysHarness) holdBeforeWrite() {
	var arrived sync.WaitGroup
	arrived.Add(2)
	release := make(chan struct{})
	go func() { arrived.Wait(); close(release) }()
	k.h.beforeAdminWrite = func() {
		arrived.Done()
		select {
		case <-release:
		case <-time.After(200 * time.Millisecond):
			arrived.Add(1)
		}
	}
}
