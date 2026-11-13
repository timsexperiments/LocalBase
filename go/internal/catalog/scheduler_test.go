package catalog

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

func TestFeedURLForDiagnostics(t *testing.T) {
	for raw, want := range map[string]string{
		"https://user:pw@feed.example.com/c.json?token=x#frag": "https://feed.example.com/c.json",
		"https://feed.example.com":                             "https://feed.example.com/",
		"not a url":                                            "<invalid>",
		"":                                                     "<invalid>",
	} {
		if got := FeedURLForDiagnostics(raw); got != want {
			t.Errorf("%q: %q, want %q", raw, got, want)
		}
	}
}

// The scheduler checks the feed once as it starts; a check that fails says so, with the feed's
// address scrubbed of its query, and the channel it returns closes once it is stopped.
func TestStartFeedSchedulerSaysAFailedCheck(t *testing.T) {
	t.Setenv(FeedURLEnv, "https://127.0.0.1:1/feed.json?token=x") // unreachable; a query to scrub
	root := filepath.Join(t.TempDir(), "root")
	var buf bytes.Buffer
	stop := make(chan struct{})
	close(stop) // the first check runs before the stop is read
	<-StartFeedScheduler(root, &Catalog{}, slog.New(slog.NewJSONHandler(&buf, nil)), stop)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("%d lines: %s", len(lines), buf.String())
	}
	var v struct {
		Level, Msg, Event string
		Attributes        struct{ URL string }
	}
	if err := json.Unmarshal([]byte(lines[0]), &v); err != nil {
		t.Fatal(err)
	}
	if v.Level != "WARN" || v.Event != "catalog_feed.refresh_failed" || v.Attributes.URL != "https://127.0.0.1:1/feed.json" ||
		v.Msg != "The remote catalog feed could not be refreshed; the last good copy stays in use." {
		t.Errorf("%s", lines[0])
	}
	if strings.Contains(buf.String(), "token=x") {
		t.Errorf("the feed's query reached the log: %s", buf.String())
	}
}
