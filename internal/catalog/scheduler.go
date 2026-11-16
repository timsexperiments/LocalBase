package catalog

import (
	"context"
	"log/slog"
	"net/url"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/obs"
)

// StartFeedScheduler keeps the remote catalog feed current (TS
// feed-scheduler): one check at start, then every hour; each check downloads
// only when the cached copy is older than a day. An update reloads the
// catalog in place. The returned channel closes once stopped.
func StartFeedScheduler(rootDir string, cat *Catalog, log *slog.Logger, stop <-chan struct{}) <-chan struct{} {
	done := make(chan struct{})
	lastError := ""
	check := func() {
		res := RefreshFeed(context.Background(), rootDir, RefreshOptions{})
		short := func(s string) string { return obs.BoundedText(s, 512) }
		switch res.Outcome {
		case "updated":
			lastError = ""
			next, err := LoadRoot(rootDir, nil)
			if err != nil {
				log.Warn("The updated catalog could not be loaded; the previous one stays in use.", "event", "catalog_feed.reload_failed",
					"category", "gateway", "component", "catalog-feed", "runtime", "gateway", slog.Group("attributes", "error", short(err.Error())))
				return
			}
			cat.Replace(next)
			log.Info("Loaded a new remote catalog feed.", "event", "catalog_feed.updated", "category", "gateway",
				"component", "catalog-feed", "runtime", "gateway", slog.Group("attributes", "url", short(FeedURLForDiagnostics(res.URL))))
		case "failed":
			if res.Error == lastError {
				return
			}
			lastError = res.Error
			log.Warn("The remote catalog feed could not be refreshed; the last good copy stays in use.", "event", "catalog_feed.refresh_failed",
				"category", "gateway", "component", "catalog-feed", "runtime", "gateway",
				slog.Group("attributes", "url", short(FeedURLForDiagnostics(res.URL)), "error", short(res.Error)))
		default:
			lastError = ""
		}
	}
	go func() {
		defer close(done)
		check()
		t := time.NewTicker(FeedCheckInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				check()
			}
		}
	}()
	return done
}

// FeedURLForDiagnostics drops credentials, query and fragment.
func FeedURLForDiagnostics(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "<invalid>"
	}
	u.User, u.RawQuery, u.Fragment, u.ForceQuery = nil, "", "", false
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String()
}
