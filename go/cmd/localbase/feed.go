package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/root"
)

// models feed status|refresh|use (TS models/commands/feed.ts). These need no
// database: they read and write the feed settings and cache under the root.

var feedSourceLabels = map[string]string{
	"default":     "the published LocalBase feed",
	"settings":    "set with `local-base models feed use`",
	"environment": "set by " + catalog.FeedURLEnv,
}

func feedStatusLines(s catalog.FeedStatus) []string {
	url := "Off"
	if s.URL != nil {
		url = *s.URL
	}
	lines := []string{fmt.Sprintf("Feed:      %s (%s)", url, feedSourceLabels[s.Source])}
	if s.Problem != nil {
		lines = append(lines, "Warning:   "+*s.Problem)
	}
	if !s.Enabled {
		return lines
	}
	if s.FetchedAt != nil {
		due := ""
		if s.Stale {
			due = " (due for a refresh)"
		}
		lines = append(lines, "Fetched:   "+*s.FetchedAt+due)
	} else {
		lines = append(lines, "Fetched:   Never; the built-in catalog is in use")
	}
	if s.CheckedAt != nil && (s.FetchedAt == nil || *s.CheckedAt != *s.FetchedAt) {
		lines = append(lines, "Checked:   "+*s.CheckedAt)
	}
	if s.Revision != nil {
		lines = append(lines, "Revision:  "+*s.Revision)
	}
	if s.FetchedAt != nil {
		skipped := ""
		if s.Skipped > 0 {
			skipped = fmt.Sprintf(" · %d skipped as invalid", s.Skipped)
		}
		lines = append(lines, fmt.Sprintf("Models:    %d in the feed · %d added (community) · %d already built in%s", s.Entries, len(s.Added), s.BuiltIn, skipped))
		if s.SchemaVersion != nil {
			if *s.SchemaVersion < 2 {
				lines = append(lines, fmt.Sprintf("Schema:    version %d (no annotations)", *s.SchemaVersion))
			} else {
				extra := ""
				if s.AnnotationsSkipped > 0 {
					extra = fmt.Sprintf(" · %d skipped as invalid", s.AnnotationsSkipped)
				}
				lines = append(lines, fmt.Sprintf("Schema:    version %d · %d annotation(s)%s", *s.SchemaVersion, s.Annotations, extra))
			}
		}
	}
	if s.LastError != nil {
		lines = append(lines, "Last error: "+*s.LastError)
	}
	return lines
}

func feedRoot(c *cli.Ctx) (string, error) {
	return root.Canonical(root.Resolve(c.Global.Root))
}

func currentFeedStatus(dir string) catalog.FeedStatus {
	cat, _ := catalog.LoadRoot(dir, nil)
	return catalog.Status(dir, cat, nil, time.Now())
}

func feedCommand() *cli.Command {
	return &cli.Command{Name: "feed", Summary: "Manage the remote catalog feed", Sub: []*cli.Command{
		{Name: "refresh", Summary: "Download the remote catalog feed now if it is due",
			Examples: []string{"local-base models feed refresh", "local-base models feed refresh --force"},
			Options:  []cli.Option{{Name: "force", Bool: true, Help: "Download even if the cached feed is less than a day old"}},
			Run: func(c *cli.Ctx) (any, error) {
				dir, err := feedRoot(c)
				if err != nil {
					return nil, err
				}
				force, _ := c.OptBool("force")
				res := catalog.RefreshFeed(context.Background(), dir, catalog.RefreshOptions{Force: force})
				st := currentFeedStatus(dir)
				switch res.Outcome {
				case "disabled":
					c.Info("The remote catalog feed is off.")
				case "fresh":
					c.Info("The cached feed is less than a day old; nothing was downloaded. Use --force to download it now.")
				case "unchanged":
					c.Info("The feed has not changed.")
				case "updated":
					c.Info("Downloaded the feed: %d community model(s) added to the catalog.", len(st.Added))
				case "failed":
					tail := " The built-in catalog stays in use."
					if st.FetchedAt != nil {
						tail = " The last good copy stays in use."
					}
					c.Error("The feed could not be refreshed: %s%s", res.Error, tail)
				}
				for _, l := range feedStatusLines(st) {
					c.Info("%s", l)
				}
				var errValue any
				if res.Error != "" {
					errValue = res.Error
				}
				return map[string]any{"outcome": res.Outcome, "error": errValue, "feed": st}, nil
			},
			ExitCodeFor: func(data any) int {
				if m, ok := data.(map[string]any); ok && m["outcome"] == "failed" {
					return 1
				}
				return 0
			}},
		{Name: "status", Summary: "Show the remote catalog feed in use and when it was fetched",
			Examples: []string{"local-base models feed status"},
			Run: func(c *cli.Ctx) (any, error) {
				dir, err := feedRoot(c)
				if err != nil {
					return nil, err
				}
				st := currentFeedStatus(dir)
				for _, l := range feedStatusLines(st) {
					c.Info("%s", l)
				}
				return map[string]any{"feed": st}, nil
			}},
		{Name: "use", Summary: "Choose the remote catalog feed URL, or turn the feed off", Usage: "<feed>", MinArgs: 1, MaxArgs: 1,
			Examples: []string{"local-base models feed use https://example.com/catalog/feed.json", "local-base models feed use off",
				"local-base models feed use default"},
			Run: func(c *cli.Ctx) (any, error) {
				dir, err := feedRoot(c)
				if err != nil {
					return nil, err
				}
				feed := c.Args[0]
				if feed != "default" && feed != "off" {
					if feed == "" {
						return nil, cli.Inputf("feed: Too small: expected string to have >=1 characters")
					}
					if p := catalog.FeedURLProblem(feed); p != "" {
						return nil, cli.Inputf("feed: %s", p)
					}
				}
				if _, err := os.Stat(filepath.Join(dir, ".localbase-root.json")); err != nil {
					return nil, cli.Inputf("%s is not a LocalBase root yet. Run `local-base init` first.", dir)
				}
				value := feed
				if feed == "off" {
					value = ""
				}
				if err := catalog.WriteFeedSettings(dir, value); err != nil {
					return nil, err
				}
				s := catalog.ReadFeedSettings(dir, nil)
				if s.Source == "environment" {
					c.Error("Saved, but %s is set and takes precedence while it is.", catalog.FeedURLEnv)
				}
				switch feed {
				case "off":
					c.Info("The remote catalog feed is off. Only the built-in and user catalogs load.")
				case "default":
					c.Info("Using the published LocalBase feed.")
				default:
					c.Info("Using the feed at %s.", feed)
				}
				if s.URL != "" && feed != "off" {
					c.Info("Run `local-base models feed refresh` to download it now; a running gateway checks within the hour.")
				}
				return map[string]any{"feed": currentFeedStatus(dir)}, nil
			}},
	}}
}
