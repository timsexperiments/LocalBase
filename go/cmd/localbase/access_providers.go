package main

import (
	"errors"
	"os"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/termsafe"
)

// permissionsOption reads --permissions; nil when omitted.
func permissionsOption(c *cli.Ctx) ([]string, error) {
	if !c.Has("permissions") {
		return nil, nil
	}
	scopes, err := parseScopes(c.String("permissions"))
	if err != nil {
		return nil, cli.Inputf("permissions: %s", strings.TrimPrefix(err.Error(), "scopes: "))
	}
	return scopes, nil
}

func originOption(c *cli.Ctx) (string, error) {
	origin, err := required(c, "origin", "origin")
	if err == nil && !auth.ValidOrigin(origin) {
		err = cli.Inputf("origin: Expected an exact HTTPS origin without a path, credentials or trailing slash")
	}
	return origin, err
}

func registrationOptions(c *cli.Ctx) (id, name string, err error) {
	if id, err = required(c, "id", "id"); err != nil {
		return
	}
	if name, err = required(c, "name", "name"); err != nil {
		return
	}
	return
}

// defaultPermissions is the permissions a provider command saves: the ones given, or when none
// were, the current set, or DefaultBrowserPermissions when browser access is off.
func defaultPermissions(current *auth.BrowserAccess, perms []string) []string {
	if perms != nil {
		return perms
	}
	if current != nil {
		return current.Permissions
	}
	return auth.DefaultBrowserPermissions
}

// planError is the command line's words for a plan's refusal; other errors pass through.
func planError(err error) error {
	var invalid *auth.ValidationError
	var exists *auth.MagicLinkExistsError
	switch {
	case errors.As(err, &invalid):
		return cli.Inputf("%s", invalid.Message)
	case errors.Is(err, auth.ErrEmailNotConfigured):
		return cli.Inputf("Configure email delivery before enabling magic-link authentication.")
	case errors.As(err, &exists):
		return cli.Inputf("Remove magic-link registration %s before adding another.", exists.ID)
	}
	return err
}

// saveRegistration upserts a registration after validating the result. A magic-link one is
// checked first: email delivery configured (a file that can't be read counts as none) and no
// other magic-link registration. It saves even when nothing changed; changed says whether it
// did, and its callers audit only a change.
func saveRegistration(e *env, reg auth.Registration, origin string, perms []string) (saved *auth.BrowserAccess, changed bool, err error) {
	err = withOperation(e, "configure browser access", func() error {
		current, err := auth.LoadAccess(e.Root.Dir)
		if err != nil {
			return err
		}
		if reg.Kind == "magic-link" {
			email, err := auth.LoadEmail(e.Root.Dir)
			if err != nil {
				email = nil
			}
			if err := auth.CheckMagicLink(current, email, reg.ID); err != nil {
				return planError(err)
			}
		}
		next, c, err := auth.PlanRegistration(current, reg, origin, defaultPermissions(current, perms))
		if err != nil {
			return planError(err)
		}
		if err := auth.SaveAccess(e.Root.Dir, next); err != nil {
			return err
		}
		saved, changed = next, c
		return nil
	})
	return saved, changed, err
}

func configured(cfg *auth.BrowserAccess) map[string]any {
	return map[string]any{"config": cfg.Summary(), "restartRequired": true}
}

func registrationList(e *env, kind string) ([]any, error) {
	current, err := auth.LoadAccess(e.Root.Dir)
	if err != nil {
		return nil, err
	}
	out := []any{}
	if current == nil || current.Provider.Kind != "direct" {
		return out, nil
	}
	provider, _ := current.Summary()["provider"].(map[string]any)
	regs, _ := provider["registrations"].([]map[string]any) // Summary builds this type
	for _, r := range regs {
		if r["kind"] == kind {
			out = append(out, r)
		}
	}
	return out, nil
}

// removeRegistration removes a registration of one kind; label names it in
// messages ("OpenID Connect registration").
func removeRegistration(c *cli.Ctx, e *env, kind, label string) (any, error) {
	id := c.Args[0]
	var result *auth.BrowserAccess
	err := withOperation(e, "configure browser access", func() error {
		current, err := auth.LoadAccess(e.Root.Dir)
		if err != nil {
			return err
		}
		found := false
		if current != nil && current.Provider.Kind == "direct" {
			for _, r := range current.Provider.Registrations {
				if r.ID == id && r.Kind == kind {
					found = true
				}
			}
		}
		if !found {
			return cli.Inputf("%s %s not found.", label, id)
		}
		next, disable, err := auth.PlanRemoveRegistration(current, id)
		if errors.Is(err, auth.ErrRegistrationNotFound) {
			return cli.Inputf("%s %s not found.", label, id)
		}
		if err != nil {
			return err
		}
		if disable {
			_, err := auth.DisableAccess(e.Root.Dir)
			return err
		}
		result = next
		return auth.SaveAccess(e.Root.Dir, next)
	})
	if err != nil {
		return nil, err
	}
	e.audit.Change("access", "remove-registration", "registration", id)
	if result == nil {
		c.Info("Removed %s and disabled browser access. Restart LocalBase to apply it.", id)
		return map[string]any{"removed": true, "config": nil, "restartRequired": true}, nil
	}
	c.Info("Removed %s %s. Restart LocalBase to apply it.", label, id)
	return map[string]any{"removed": true, "config": result.Summary(), "restartRequired": true}, nil
}

// accessProviderCommands are the access commands for sign-in providers: show, cloudflare, oidc, github, magic-link and disable.
func accessProviderCommands() []*cli.Command {
	permissions := cli.Option{Name: "permissions", Value: "<permission,...>", Help: "Browser permissions; preserves the current set when omitted"}
	origin := cli.Option{Name: "origin", Value: "<https://localbase.example.com>", Help: "Exact public UI origin"}
	idPositional := func(name, summary, kind, label string) *cli.Command {
		return &cli.Command{Name: name, Summary: summary, Usage: "<provider-id>", MinArgs: 1, MaxArgs: 1,
			Run: withEnv(func(c *cli.Ctx, e *env) (any, error) { return removeRegistration(c, e, kind, label) })}
	}
	list := func(summary, kind, noun string) *cli.Command {
		return &cli.Command{Name: "list", Summary: summary, Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
			regs, err := registrationList(e, kind)
			if err != nil {
				return nil, err
			}
			c.Info("%d %s registrations.", len(regs), noun)
			return map[string]any{"registrations": regs}, nil
		})}
	}
	return []*cli.Command{
		{Name: "show", Summary: "Show browser identity provider configuration", Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
			cfg, err := auth.LoadAccess(e.Root.Dir)
			if err != nil {
				return nil, err
			}
			if cfg == nil {
				c.Info("Browser access is not configured.")
				return map[string]any{"config": nil}, nil
			}
			c.Info("Browser access: %s for %s", cfg.Provider.Kind, termsafe.PrintableASCII(cfg.Origin))
			return map[string]any{"config": cfg.Summary()}, nil
		})},
		{Name: "cloudflare", Summary: "Configure Cloudflare Access browser authentication", Options: []cli.Option{
			{Name: "team-domain", Value: "<team.cloudflareaccess.com>", Help: "Cloudflare Access team domain"},
			{Name: "audience", Value: "<AUD tag>", Help: "Cloudflare Access application audience"},
			origin, permissions,
		}, Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
			team, err := required(c, "team-domain", "teamDomain")
			if err != nil {
				return nil, err
			}
			audience, err := required(c, "audience", "audience")
			if err != nil {
				return nil, err
			}
			o, err := originOption(c)
			if err != nil {
				return nil, err
			}
			perms, err := permissionsOption(c)
			if err != nil {
				return nil, err
			}
			var cfg *auth.BrowserAccess
			var changed bool
			err = withOperation(e, "configure browser access", func() error {
				current, err := auth.LoadAccess(e.Root.Dir)
				if err != nil {
					return err
				}
				// It saves even when nothing changed, and audits only a change.
				cfg, changed, err = auth.PlanCloudflare(current, team, audience, o, defaultPermissions(current, perms))
				if err != nil {
					return planError(err)
				}
				return auth.SaveAccess(e.Root.Dir, cfg)
			})
			if err != nil {
				return nil, err
			}
			if changed {
				e.audit.Change("access", "configure-cloudflare", "provider", "browser-access")
			}
			c.Info("Saved Cloudflare Access configuration. Restart LocalBase to apply it.")
			return configured(cfg), nil
		})},
		{Name: "oidc", Summary: "Manage OpenID Connect registrations", Sub: []*cli.Command{
			{Name: "add", Summary: "Add or update an OpenID Connect registration", Options: []cli.Option{
				{Name: "id", Value: "<provider-id>", Help: "Stable lowercase registration ID"},
				{Name: "name", Value: "<Provider name>", Help: "Name shown on the sign-in page"},
				{Name: "issuer", Value: "<https://identity.example.com>", Help: "Exact OpenID Connect issuer"},
				{Name: "client-id", Value: "<CLIENT_ID>", Help: "OpenID Connect client ID"},
				{Name: "client-secret-env", Value: "<ENVIRONMENT_VARIABLE>", Help: "Environment variable containing the client secret"},
				{Name: "public-client", Bool: true, Help: "Configure an OpenID Connect public client"},
				origin, permissions,
			}, Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
				id, name, err := registrationOptions(c)
				if err != nil {
					return nil, err
				}
				issuer, err := required(c, "issuer", "issuer")
				if err != nil {
					return nil, err
				}
				clientID, err := required(c, "client-id", "clientId")
				if err != nil {
					return nil, err
				}
				o, err := originOption(c)
				if err != nil {
					return nil, err
				}
				perms, err := permissionsOption(c)
				if err != nil {
					return nil, err
				}
				secretEnv := c.String("client-secret-env")
				if c.Has("client-secret-env") && !envNameRe.MatchString(secretEnv) {
					return nil, cli.Inputf("clientSecretEnv: Invalid string: must match pattern /^[A-Za-z_][A-Za-z0-9_]*$/")
				}
				if c.Bool("public-client") == c.Has("client-secret-env") {
					return nil, cli.Inputf("input: Choose exactly one of --public-client or --client-secret-env.")
				}
				clientAuth := &auth.ClientAuth{Kind: "none"}
				if !c.Bool("public-client") {
					secret := os.Getenv(secretEnv)
					if secret == "" {
						return nil, cli.Inputf("OpenID Connect client secret environment variable %s is empty or unavailable.", secretEnv)
					}
					clientAuth = &auth.ClientAuth{Kind: "client-secret-basic", ClientSecret: secret}
				}
				reg := auth.Registration{Kind: "oidc", ID: id, Name: name, Issuer: issuer, ClientID: clientID, ClientAuthentication: clientAuth}
				cfg, changed, err := saveRegistration(e, reg, o, perms)
				if err != nil {
					return nil, err
				}
				if changed {
					e.audit.Change("access", "upsert-oidc", "registration", id)
				}
				c.Info("Saved OpenID Connect registration %s. Restart LocalBase to apply it.", id)
				return configured(cfg), nil
			})},
			list("List OpenID Connect registrations", "oidc", "OIDC"),
			idPositional("remove", "Remove an OpenID Connect registration", "oidc", "OpenID Connect registration"),
		}},
		{Name: "github", Summary: "Manage GitHub OAuth registrations", Sub: []*cli.Command{
			list("List GitHub OAuth registrations", "github-oauth", "GitHub OAuth"),
			idPositional("remove", "Remove a GitHub OAuth registration", "github-oauth", "GitHub OAuth registration"),
		}},
		{Name: "magic-link", Summary: "Manage native magic-link authentication", Sub: []*cli.Command{
			{Name: "add", Summary: "Add or update native magic-link authentication", Options: []cli.Option{
				{Name: "id", Value: "<email>", Help: "Stable lowercase registration ID"},
				{Name: "name", Value: "<Email>", Help: "Name shown on the sign-in page"},
				origin, permissions,
			}, Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
				id, name, err := registrationOptions(c)
				if err != nil {
					return nil, err
				}
				o, err := originOption(c)
				if err != nil {
					return nil, err
				}
				perms, err := permissionsOption(c)
				if err != nil {
					return nil, err
				}
				cfg, changed, err := saveRegistration(e, auth.Registration{Kind: "magic-link", ID: id, Name: name}, o, perms)
				if err != nil {
					return nil, err
				}
				if changed {
					e.audit.Change("access", "upsert-magic-link", "registration", id)
				}
				c.Info("Saved magic-link registration %s. Restart LocalBase to apply it.", id)
				return configured(cfg), nil
			})},
			list("List magic-link registrations", "magic-link", "magic-link"),
			idPositional("remove", "Remove native magic-link authentication", "magic-link", "Magic-link registration"),
		}},
		{Name: "disable", Summary: "Disable browser authentication", Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
			var disabled bool
			err := withOperation(e, "configure browser access", func() (err error) {
				disabled, err = auth.DisableAccess(e.Root.Dir)
				return err
			})
			if err != nil {
				return nil, err
			}
			if disabled {
				e.audit.Change("access", "disable", "provider", "browser-access")
				c.Info("Disabled browser access. Restart LocalBase to apply it.")
			} else {
				c.Info("Browser access was already disabled. Restart LocalBase to apply it.")
			}
			return map[string]any{"disabled": disabled, "restartRequired": disabled}, nil
		})},
	}
}
