package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/termsafe"
)

// accessPolicyCommands are the access commands for the authorization policy and its bindings.
func accessPolicyCommands() []*cli.Command {
	ctx := context.Background()
	return []*cli.Command{
		{Name: "policy", Summary: "Manage browser authorization policy", Sub: []*cli.Command{
			{Name: "show", Summary: "Show the browser authorization policy", Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
				policy, err := auth.LoadPolicy(ctx, e.Store.DB)
				if err != nil {
					return nil, err
				}
				if policy == nil {
					c.Info("Browser access policy is not configured.")
					return map[string]any{"policy": nil}, nil
				}
				c.Info("Browser access policy: %d roles, %d bindings.", len(policy.Roles), len(policy.Bindings))
				return map[string]any{"policy": policy}, nil
			})},
			{Name: "apply", Summary: "Atomically apply a browser authorization policy", Options: []cli.Option{
				{Name: "file", Value: "<policy.json>", Help: "Complete policy JSON file"},
			}, Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
				file, err := required(c, "file", "file")
				if err != nil {
					return nil, err
				}
				if file == "" {
					return nil, cli.Inputf("file: Too small: expected string to have >=1 characters")
				}
				b, err := os.ReadFile(file)
				if err != nil {
					return nil, cli.Inputf("Unable to read access policy: %s", file)
				}
				var policy auth.Policy
				if !json.Valid(b) {
					return nil, cli.Inputf("Access policy must be valid JSON.")
				}
				if err := strictJSON(b, &policy); err != nil {
					return nil, cli.Inputf("Invalid access policy: %s", err.Error())
				}
				// The policy itself is normalized, not a copy: binding its issuers, the words of
				// a refusal and what is applied all see it as it will be stored.
				if err := policy.Normalize(); err != nil {
					return nil, cli.Inputf("Invalid access policy: %s", err.Error())
				}
				warn := noManagerWarning(c, e)
				open := ""
				err = withOperation(e, "configure browser access", func() error {
					cfg, err := auth.LoadAccess(e.Root.Dir)
					if err != nil {
						return err
					}
					if cfg == nil {
						return cli.Inputf("Configure a browser identity provider first.")
					}
					if err := policy.BindIssuers(cfg.Issuers(), nil); err != nil {
						return err
					}
					open = auth.OpenSignIn(cfg, &policy)
					return auth.ApplyPolicy(ctx, e.Store, &policy)
				})
				if err != nil {
					return nil, accessError(err)
				}
				e.audit.PolicyApplied(&policy)
				c.Info("Saved browser access policy. Changes apply immediately.")
				if open != "" {
					c.Info("%s", open)
				}
				warn()
				return map[string]any{"policy": &policy, "restartRequired": false}, nil
			})},
			{Name: "starter", Summary: "Give one address every permission, and nobody else anything", Options: []cli.Option{
				{Name: "email", Value: "<person@example.com>", Help: "The verified address that gets every permission"},
				{Name: "issuer", Value: "<https://identity.example.com>", Help: "The sign-in provider that address uses; needed when several are configured"},
			}, Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
				email, err := emailOption(c, "email", "email")
				if err != nil {
					return nil, err
				}
				var policy *auth.Policy
				err = withOperation(e, "configure browser access", func() error {
					cfg, err := auth.LoadAccess(e.Root.Dir)
					if err != nil {
						return err
					}
					if cfg == nil {
						return cli.Inputf("Configure a browser identity provider first.")
					}
					existing, err := auth.LoadPolicy(ctx, e.Store.DB)
					if err != nil {
						return err
					}
					if existing != nil {
						return cli.Inputf("A browser access policy already exists. Change it with local-base access policy apply, or invite someone with local-base access users invite.")
					}
					issuer, err := auth.InviteIssuer(c.String("issuer"), email, cfg.Issuers(), nil)
					if err != nil {
						return err
					}
					policy = auth.StarterPolicy(email, issuer)
					return auth.ApplyPolicy(ctx, e.Store, policy)
				})
				if err != nil {
					return nil, accessError(err)
				}
				e.audit.PolicyApplied(policy)
				c.Info("Saved browser access policy: %s has every permission, and nobody else has anything. Changes apply immediately.", email)
				return map[string]any{"policy": policy, "restartRequired": false}, nil
			})},
			{Name: "test", Summary: "Evaluate a browser identity against the local policy", Options: []cli.Option{
				{Name: "issuer", Value: "<https://identity.example.com>", Help: "Verified identity issuer"},
				{Name: "subject", Value: "<subject>", Help: "Exact verified subject"},
				{Name: "email", Value: "<person@example.com>", Help: "Verified email address"},
			}, Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
				issuer, err := required(c, "issuer", "issuer")
				if err != nil {
					return nil, err
				}
				subject, err := required(c, "subject", "subject")
				if err != nil {
					return nil, err
				}
				if subject == "" || len(subject) > 512 {
					return nil, cli.Inputf("subject: Invalid subject")
				}
				id := auth.Identity{Issuer: issuer, Subject: subject}
				if c.Has("email") {
					if id.VerifiedEmail, err = emailOption(c, "email", "email"); err != nil {
						return nil, err
					}
				}
				cfg, err := auth.LoadAccess(e.Root.Dir)
				if err != nil {
					return nil, err
				}
				if cfg == nil {
					return nil, cli.Inputf("Configure a browser identity provider first.")
				}
				d, err := auth.ResolvePolicy(ctx, e.Store, id, false)
				if err != nil {
					return nil, err
				}
				if d == nil {
					d = &auth.Decision{MatchedRoles: []string{}, Permissions: cfg.Permissions}
				}
				policy, err := auth.LoadPolicy(ctx, e.Store.DB)
				if err != nil {
					return nil, err
				}
				c.Info("Matched %d roles and %d permissions.", len(d.MatchedRoles), len(d.Permissions))
				return map[string]any{"policyConfigured": policy != nil, "matchedRoles": d.MatchedRoles, "permissions": d.Permissions}, nil
			})},
			{Name: "clear", Summary: "Clear the local policy and use provider-wide permissions", Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
				var cleared bool
				warn := noManagerWarning(c, e)
				err := withOperation(e, "configure browser access", func() (err error) {
					if cfg, err := auth.LoadAccess(e.Root.Dir); err != nil {
						return err
					} else if cfg == nil {
						return cli.Inputf("Configure a browser identity provider first.")
					}
					cleared, err = auth.ClearPolicy(ctx, e.Store)
					return err
				})
				if err != nil {
					return nil, accessError(err)
				}
				if cleared {
					e.audit.Change("access", "clear-policy", "policy", "")
					c.Info("Cleared browser access policy. Provider-wide permissions now apply.")
					warn()
				} else {
					c.Info("Browser access policy was already clear.")
				}
				return map[string]any{"cleared": cleared, "restartRequired": false}, nil
			})},
		}},
		{Name: "bindings", Summary: "Name the sign-in provider on older email bindings and invitations", Sub: []*cli.Command{
			{Name: "adopt", Summary: "Give every email binding and invitation that names no provider this one", Options: []cli.Option{
				{Name: "issuer", Value: "<https://identity.example.com>", Help: "A configured sign-in provider's issuer"},
			}, Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
				issuer, err := required(c, "issuer", "issuer")
				if err != nil {
					return nil, err
				}
				var adopted int
				err = withOperation(e, "configure browser access", func() error {
					cfg, err := auth.LoadAccess(e.Root.Dir)
					if err != nil {
						return err
					}
					adopted, err = auth.AdoptBindingIssuer(ctx, e.Store, cfg, issuer)
					return err
				})
				if err != nil {
					return nil, accessError(err)
				}
				if adopted > 0 {
					e.audit.Change("access", "adopt-binding-issuer", "issuer", issuer, "count", adopted)
				}
				c.Info("%d email bindings and invitations now trust %s.", adopted, termsafe.PrintableASCII(issuer))
				return map[string]any{"adopted": adopted, "issuer": issuer}, nil
			})},
		}},
	}
}

// strictJSON decodes rejecting unknown fields.
func strictJSON(b []byte, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s", strings.TrimPrefix(err.Error(), "json: "))
	}
	return nil
}
