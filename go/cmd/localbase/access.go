package main

import (
	"context"
	"errors"
	"regexp"
	"slices"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/lease"
)

// `access`: browser sign-in providers, email delivery, the authorization
// policy and managed users (TS auth/commands/access). GitHub OAuth is not
// offered (DESIGN §11); existing GitHub registrations can still be listed
// and removed.

var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// required reads an option the TS schema requires, with zod's message.
func required(c *cli.Ctx, option, field string) (string, error) {
	if !c.Has(option) {
		return "", cli.Inputf("%s: Invalid input: expected string, received undefined", field)
	}
	return c.String(option), nil
}

// withOperation runs fn holding the root operation lease.
func withOperation(e *env, operation string, fn func() error) error {
	return lease.WithOperation(e.Root.Dir, operation, fn)
}

func emailOption(c *cli.Ctx, option, field string) (string, error) {
	v, err := required(c, option, field)
	if err != nil {
		return "", err
	}
	if auth.ValidEmail(v) == "" {
		return "", cli.Inputf("%s: Invalid email address", field)
	}
	return v, nil
}

// accessError turns domain errors into their messages.
func accessError(err error) error {
	var ae *auth.AccessError
	if errors.As(err, &ae) {
		return errors.New(ae.Message)
	}
	return err
}

func accessCommand() *cli.Command {
	sub := slices.Concat(accessProviderCommands(), accessEmailCommands(), accessPolicyCommands(), accessUserCommands())
	return ordered(&cli.Command{Name: "access", Summary: "Manage browser access", Sub: sub}, "show", "cloudflare", "github", "magic-link", "email", "oidc", "disable", "policy", "bindings", "users", "login-link", "sessions")
}

// ordered puts a group's subcommands in the TS help order.
func ordered(c *cli.Command, names ...string) *cli.Command {
	var out []*cli.Command
	for _, n := range names {
		if s := c.Sub; s != nil {
			for _, sub := range s {
				if sub.Name == n {
					out = append(out, sub)
				}
			}
		}
	}
	c.Sub = out
	return c
}

// noManagerWarning reads whether anyone can manage access now, and returns a
// function that, after a change, warns when the change left nobody able to.
// The API refuses such changes; these commands are the recovery path, so
// they only warn.
func noManagerWarning(c *cli.Ctx, e *env) func() {
	before := hasManager(e)
	return func() {
		if before && !hasManager(e) {
			c.Error("Warning: nobody can manage access now: no active person, binding or API key has access:manage. This command is the recovery path; restore it with `local-base access users roles`, `local-base access policy apply` or `local-base keys create --scopes access:manage`.")
		}
	}
}

func hasManager(e *env) bool {
	cfg, err := auth.LoadAccess(e.Root.Dir)
	if err != nil {
		return false
	}
	g, err := auth.LoadGuard(context.Background(), e.Store, cfg)
	return err == nil && g.HasManager()
}
