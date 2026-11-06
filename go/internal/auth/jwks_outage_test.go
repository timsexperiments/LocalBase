package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// keyEndpoint is a provider's key endpoint that fails until good is set, counting what it's asked.
type keyEndpoint struct {
	*httptest.Server
	asked atomic.Int64
	good  atomic.Bool
}

func newKeyEndpoint(t *testing.T) *keyEndpoint {
	t.Helper()
	e := &keyEndpoint{}
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		e.asked.Add(1)
		if !e.good.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		x := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "OKP", "kid": "the-key", "crv": "Ed25519", "x": x}}})
	}))
	t.Cleanup(e.Close)
	return e
}

// callAtOnce asks for a key from many callers together.
func callAtOnce(set *JWKS, kid string) (errs, found int64) {
	var failed, keys atomic.Int64
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := set.candidates(context.Background(), kid)
			if err != nil {
				failed.Add(1)
			}
			if len(got) > 0 {
				keys.Add(1)
			}
		}()
	}
	wg.Wait()
	return failed.Load(), keys.Load()
}

// Each caller used to wait behind a failing fetch of its own, one after another, so an outage
// held up everyone in turn.
func TestAFailingKeyEndpointIsAskedOnce(t *testing.T) {
	e := newKeyEndpoint(t)
	set := &JWKS{URL: e.URL}
	errs, _ := callAtOnce(set, "the-key")
	if n := e.asked.Load(); n != 1 {
		t.Errorf("16 callers asked it %d times", n)
	}
	if errs != 16 {
		t.Errorf("%d of 16 callers were told it failed", errs)
	}
}

// Keys it already holds go on working, without waiting for the endpoint each time.
func TestKeysHeldKeepWorkingWhileTheEndpointFails(t *testing.T) {
	e := newKeyEndpoint(t)
	e.good.Store(true)
	set := &JWKS{URL: e.URL}
	if _, err := set.candidates(context.Background(), "the-key"); err != nil {
		t.Fatal(err)
	}
	e.good.Store(false)
	e.asked.Store(0)
	set.fetched = time.Now().Add(-11 * time.Minute)
	errs, found := callAtOnce(set, "the-key")
	if n := e.asked.Load(); n != 1 {
		t.Errorf("16 callers asked it %d times", n)
	}
	if errs != 0 || found != 16 {
		t.Errorf("%d errors, and %d of 16 callers got the key", errs, found)
	}
}

// A key it has never heard of doesn't get past the wait either, so tokens naming made-up keys can't
// keep asking during an outage.
func TestAnUnknownKeyWaitsDuringAnOutage(t *testing.T) {
	e := newKeyEndpoint(t)
	e.good.Store(true)
	set := &JWKS{URL: e.URL}
	if _, err := set.candidates(context.Background(), "the-key"); err != nil {
		t.Fatal(err)
	}
	e.good.Store(false)
	e.asked.Store(0)
	set.fetched = time.Now().Add(-time.Minute)
	if _, found := callAtOnce(set, "made-up"); found != 0 {
		t.Errorf("%d callers got a key for a made-up name", found)
	}
	if n := e.asked.Load(); n != 1 {
		t.Errorf("16 callers asked it %d times", n)
	}
}

// It asks again once the wait is over, and not before.
func TestTheWaitAfterAFailureIsTheStatedOne(t *testing.T) {
	e := newKeyEndpoint(t)
	set := &JWKS{URL: e.URL}
	if _, err := set.candidates(context.Background(), "the-key"); err == nil {
		t.Fatal("no error from a failing endpoint")
	}
	set.failed = time.Now().Add(-jwksRetryWait + time.Second)
	if _, err := set.candidates(context.Background(), "the-key"); err == nil {
		t.Error("no error during the wait")
	}
	if n := e.asked.Load(); n != 1 {
		t.Errorf("it asked %d times before the wait was over", n)
	}
	e.good.Store(true)
	set.failed = time.Now().Add(-jwksRetryWait - time.Second)
	got, err := set.candidates(context.Background(), "the-key")
	if err != nil || len(got) != 1 {
		t.Errorf("after the wait: %d keys, %v", len(got), err)
	}
	if n := e.asked.Load(); n != 2 {
		t.Errorf("it asked %d times in all, want 2", n)
	}
	if jwksRetryWait != 30*time.Second {
		t.Errorf("the wait is %v, and DESIGN says 30 s", jwksRetryWait)
	}
}

// Someone who hangs up doesn't turn the fetch into a failure that everyone else then has to wait out.
func TestACallerLeavingCostsTheOthersNothing(t *testing.T) {
	e := newKeyEndpoint(t)
	e.good.Store(true)
	set := &JWKS{URL: e.URL}
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := set.candidates(gone, "the-key"); err != nil || len(got) != 1 {
		t.Errorf("the caller who left: %d keys, %v", len(got), err)
	}
	if got, err := set.candidates(context.Background(), "the-key"); err != nil || len(got) != 1 {
		t.Errorf("the next caller: %d keys, %v", len(got), err)
	}
	if n := e.asked.Load(); n != 1 {
		t.Errorf("it asked %d times", n)
	}
}

// An endpoint that never answers can't hold everyone up for longer than the limit on one fetch,
// whatever client it was given.
func TestAKeyEndpointThatHangsIsGivenUpOn(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
		}
	}))
	defer srv.Close()
	defer close(release)
	set := &JWKS{URL: srv.URL, Client: &http.Client{}, fetchLimit: 50 * time.Millisecond}
	began := time.Now()
	if _, err := set.candidates(context.Background(), "the-key"); err == nil {
		t.Error("no error from an endpoint that never answered")
	}
	if took := time.Since(began); took > 10*time.Second {
		t.Errorf("it waited %v", took)
	}
	if jwksFetchLimit != 5*time.Second {
		t.Errorf("the limit is %v, and DESIGN says 5 s", jwksFetchLimit)
	}
}
