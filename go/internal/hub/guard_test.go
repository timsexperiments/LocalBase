package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// The Hub endpoint may be a private mirror the person named. Where it redirects is
// judged like anything else, and the token does not go there.
func TestHubRedirectIsJudged(t *testing.T) {
	var reached, token atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		if r.Header.Get("Authorization") != "" {
			token.Add(1)
		}
	}))
	defer target.Close()
	away := strings.Replace(target.URL, "127.0.0.1", "localhost", 1)
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/moved" {
			http.Redirect(w, r, away+"/x", http.StatusFound)
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer hubSrv.Close()
	c := &Client{Root: t.TempDir(), Endpoint: hubSrv.URL, Token: "hf_secret"}
	if _, _, err := c.getJSON(context.Background(), hubSrv.URL+"/moved", "look"); err == nil || !strings.Contains(err.Error(), "refusing to connect") {
		t.Errorf("redirected: %v", err)
	}
	if reached.Load() != 0 || token.Load() != 0 {
		t.Errorf("reached %d, with the token %d", reached.Load(), token.Load())
	}
	if _, _, err := c.getJSON(context.Background(), hubSrv.URL+"/ok", "look"); err != nil {
		t.Errorf("the named endpoint: %v", err)
	}
}
