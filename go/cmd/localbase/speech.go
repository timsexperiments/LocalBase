package main

import (
	"context"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
)

// speechCommand holds speech settings. `speech marking` is the single switch
// for CrispASR's AI-content marking (watermark, C2PA, spoken disclaimer); it
// is on unless set to off. A running gateway restarts its speech server on
// the next request.
func speechCommand() *cli.Command {
	ctx := context.Background()
	return &cli.Command{Name: "speech", Summary: "Speech settings", Sub: []*cli.Command{
		{Name: "marking", Summary: "Show or set AI-content marking of synthesized speech (on or off)", Usage: "[on|off]", MaxArgs: 1,
			Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
				if len(c.Args) == 1 {
					v := c.Args[0]
					if v != "on" && v != "off" {
						return nil, cli.Inputf("marking must be on or off")
					}
					if err := e.Store.SetSetting(ctx, runtime.SpeechMarkingKey, v); err != nil {
						return nil, err
					}
				}
				on := runtime.SpeechMarking(ctx, e.Store)
				state := map[bool]string{true: "on", false: "off"}[on]
				if on {
					c.Info("Speech marking: on (CrispASR adds its AI-content watermark).")
				} else {
					c.Info("Speech marking: off (CrispASR runs with --accept-marking-responsibility; disclosure is the operator's responsibility).")
				}
				return map[string]any{"marking": state}, nil
			})},
	}}
}
