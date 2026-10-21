package main

import (
	"context"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"syscall"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/root"
)

var requestIDFlagRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

func logsCommand() *cli.Command {
	return &cli.Command{Name: "logs", Summary: "Read structured LocalBase operational logs",
		Examples: []string{"local-base logs --level error", "local-base logs --follow --runtime llm"},
		Options: []cli.Option{
			{Name: "follow", Bool: true, Help: "Continue streaming new events"},
			{Name: "limit", Value: "<count>", Help: "Maximum snapshot events (default 200, max 5000)"},
			{Name: "since", Value: "<ISO-8601>", Help: "Include events at or after this timestamp"},
			{Name: "level", Value: "<debug|info|warn|error>", Help: "Filter by severity"},
			{Name: "runtime", Value: "<gateway|llm|stt|tts|image|service|cli>", Help: "Filter by runtime"},
			{Name: "request-id", Value: "<id>", Help: "Filter by request ID"},
		},
		Run: func(c *cli.Ctx) (any, error) {
			var f obs.Filters
			limit := 200
			if n, ok, err := c.Int("limit"); err != nil || (ok && (n < 1 || n > 5000)) {
				return nil, cli.Inputf("limit: must be an integer from 1 to 5000")
			} else if ok {
				limit = n
			}
			if s := c.String("since"); s != "" {
				t, err := time.Parse(time.RFC3339Nano, s)
				if err != nil {
					return nil, cli.Inputf("since: Invalid ISO datetime")
				}
				f.Since = t
			}
			if f.Level = c.String("level"); f.Level != "" && !slices.Contains([]string{"debug", "info", "warn", "error"}, f.Level) {
				return nil, cli.Inputf(`level: Invalid option: expected one of "debug"|"info"|"warn"|"error"`)
			}
			if f.Runtime = c.String("runtime"); f.Runtime != "" &&
				!slices.Contains([]string{"gateway", "llm", "stt", "tts", "image", "video", "service", "cli"}, f.Runtime) {
				return nil, cli.Inputf(`runtime: Invalid option: expected one of "gateway"|"llm"|"stt"|"tts"|"image"|"video"|"service"|"cli"`)
			}
			if f.RequestID = c.String("request-id"); f.RequestID != "" && !requestIDFlagRe.MatchString(f.RequestID) {
				return nil, cli.Inputf("requestId: Invalid string")
			}
			r, err := root.Open(root.Resolve(c.Global.Root))
			if err != nil {
				return nil, err
			}
			if !c.Bool("follow") {
				events, err := obs.Snapshot(r.Dir, f, limit)
				if err != nil {
					return nil, err
				}
				raw := make([]any, len(events))
				for i, e := range events {
					raw[i] = e.Raw
					if !c.Global.JSON {
						c.Info("%s", obs.Human(e))
					}
				}
				return map[string]any{"events": raw}, nil
			}
			c.Streaming()
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return nil, obs.Follow(ctx, r.Dir, f, func(e *obs.Event) error {
				if c.Global.JSON {
					_, err := os.Stdout.Write(append(append([]byte{}, e.Raw...), '\n'))
					return err
				}
				c.Info("%s", obs.Human(e))
				return nil
			})
		}}
}
