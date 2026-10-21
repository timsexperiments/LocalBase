package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/voices"
)

// cliCreator attributes voices created from the CLI to the local operator.
const cliCreator = "cli"

func describeVoice(v *voices.Voice) string {
	by := v.CreatedBy
	if by == "" {
		by = "unknown"
	}
	return fmt.Sprintf("%s | %s | %s s | created=%s | by=%s", v.ID, v.Name,
		voices.ToFixed1(float64(v.DurationMs)/1000), v.CreatedAt, by)
}

// voiceCLIError turns a rejected voice operation into an input error.
func voiceCLIError(err error) error {
	var ve *voices.Error
	if errors.As(err, &ve) {
		return cli.Inputf("%s", ve.Message)
	}
	return err
}

func voicesCommand() *cli.Command {
	ctx := context.Background()
	return &cli.Command{Name: "voices", Summary: "Manage custom TTS voices", Sub: []*cli.Command{
		{Name: "list", Summary: "List custom TTS voices", Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
			list, err := voices.New(e.Store, e.Root.Dir).List(ctx)
			if err != nil {
				return nil, err
			}
			if len(list) == 0 {
				c.Info("No custom voices. Add one with: local-base voices add clip.wav --name <name> --consent")
			}
			for i := range list {
				c.Info("%s", describeVoice(&list[i]))
			}
			return map[string]any{"voices": list}, nil
		})},
		{Name: "add", Summary: "Add a custom TTS voice from a 3 to 30 second WAV recording", Usage: "<file>", MinArgs: 1, MaxArgs: 1,
			Options: []cli.Option{
				{Name: "name", Value: "<name>", Help: "Display name"},
				{Name: "consent", Bool: true, Help: "Confirm the voice is yours or its speaker allowed it to be cloned"},
			}, Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
				name := c.String("name")
				if !c.Has("name") || name == "" {
					return nil, cli.Inputf("name: Invalid input: expected string, received undefined")
				}
				if consent, _ := c.OptBool("consent"); !consent {
					return nil, cli.Inputf("Pass --consent to confirm: %s", voices.ConsentStatement)
				}
				file := c.Args[0]
				info, err := os.Stat(file)
				if err != nil || !info.Mode().IsRegular() {
					return nil, cli.Inputf("Cannot read audio file: %s", file)
				}
				if info.Size() > voices.MaxUploadBytes {
					return nil, cli.Inputf("Audio exceeds the %d MiB limit.", voices.MaxUploadBytes/(1024*1024))
				}
				audio, err := os.ReadFile(file)
				if err != nil {
					return nil, cli.Inputf("Cannot read audio file: %s", file)
				}
				v, err := voices.New(e.Store, e.Root.Dir).Create(ctx, name, true, audio, cliCreator)
				if err != nil {
					return nil, voiceCLIError(err)
				}
				c.Info("Added voice %s", describeVoice(v))
				c.Info(`Use it with "voice": "%s" on /v1/audio/speech.`, v.ID)
				return map[string]any{"voice": v}, nil
			})},
		{Name: "remove", Summary: "Delete a custom TTS voice and its recording", Usage: "<voiceId>", MinArgs: 1, MaxArgs: 1,
			Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
				v, err := voices.New(e.Store, e.Root.Dir).Remove(ctx, c.Args[0])
				if err != nil {
					return nil, voiceCLIError(err)
				}
				c.Info("Removed voice %s (%s)", v.ID, v.Name)
				return map[string]any{"voice": v}, nil
			})},
	}}
}
