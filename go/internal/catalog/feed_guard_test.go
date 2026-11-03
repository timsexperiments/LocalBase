package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// The person named the feed's host, but not wherever it redirects to.
func TestFeedRedirectIsJudged(t *testing.T) {
	var reached atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
	defer target.Close()
	away := strings.Replace(target.URL, "127.0.0.1", "localhost", 1)
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/moved" {
			http.Redirect(w, r, away+"/feed.json", http.StatusFound)
			return
		}
		reached.Add(1)
	}))
	defer feed.Close()
	refresh := func(path string) RefreshResult {
		return RefreshFeed(context.Background(), t.TempDir(), RefreshOptions{Force: true, Getenv: func(k string) string {
			if k == FeedURLEnv {
				return feed.URL + path
			}
			return ""
		}})
	}
	if res := refresh("/moved"); res.Outcome != "failed" || !strings.Contains(res.Error, "refusing to connect") || reached.Load() != 0 {
		t.Errorf("redirected: %+v, reached %d", res, reached.Load())
	}
	refresh("/feed.json")
	if reached.Load() != 1 {
		t.Errorf("the named host itself was not reached: %d", reached.Load())
	}
}
