package system

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/test/harness"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// A signed feed URL carries its secret in the query, under whatever name the signer chose, and the
// log is something people share. So the log names the feed's address and not the query, whether
// the refresh worked or failed. The name here isn't one the log's redaction knows.
func TestAFeedURLsQueryStaysOutOfTheLog(t *testing.T) {
	if harness.Impl() != "go" && harness.Impl() != "both" {
		t.Skip("runs the Go gateway")
	}
	const planted = "planted-feed-query-value"
	for _, c := range []struct {
		name, event string
		answer      func(w http.ResponseWriter)
	}{
		{"it works", "catalog_feed.updated", func(w http.ResponseWriter) { w.Write(testFeed(t)) }},
		{"it fails", "catalog_feed.refresh_failed", func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := rangeServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { c.answer(w) }), false)
			tmp := fsguard.ScratchDir(t)
			e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp, noAuth: true,
				env: []string{"LOCALBASE_CATALOG_FEED_URL=" + srv.URL + "/feed.json?signature=" + planted}}
			if r := e.cli(nil, "--json", "init"); r.code != 0 {
				t.Fatalf("init: %s %s", r.stdout, r.stderr)
			}
			e.serve("--no-auth")
			defer e.stop()
			events := filepath.Join(e.root, "logs", "events.jsonl")
			deadline := time.Now().Add(15 * time.Second)
			var logged string
			for !strings.Contains(logged, c.event) {
				if time.Now().After(deadline) {
					t.Fatalf("no %s in the log: %.500s", c.event, logged)
				}
				time.Sleep(50 * time.Millisecond)
				b, _ := os.ReadFile(events)
				logged = string(b)
			}
			if strings.Contains(logged, planted) {
				t.Error("the log has the feed URL's query")
			}
			if !strings.Contains(logged, "/feed.json") {
				t.Errorf("the log doesn't say which feed: %.500s", logged)
			}
		})
	}
}
