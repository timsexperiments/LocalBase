package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/root"
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

// withOperation runs fn holding the root operation lease.
func withOperation(e *env, operation string, fn func() error) error {
	return root.WithOperation(e.Root.Dir, operation, fn)
}

// saveRegistration upserts a registration after validating the result.
func saveRegistration(e *env, reg auth.Registration, origin string, perms []string, check func(*auth.BrowserAccess) error) (*auth.BrowserAccess, error) {
	var saved *auth.BrowserAccess
	err := withOperation(e, "configure browser access", func() error {
		current, err := auth.LoadAccess(e.Root.Dir)
		if err != nil {
			return err
		}
		if check != nil {
			if err := check(current); err != nil {
				return err
			}
		}
		if perms == nil {
			perms = auth.DefaultBrowserPermissions
			if current != nil {
				perms = current.Permissions
			}
		}
		next := auth.UpsertRegistration(current, reg, origin, perms)
		if err := next.Validate(); err != nil {
			return cli.Inputf("%s", err.Error())
		}
		if err := auth.SaveAccess(e.Root.Dir, next); err != nil {
			return err
		}
		saved = next
		return nil
	})
	return saved, err
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
		state, next := auth.RemoveRegistration(current, id)
		if state == "disabled" {
			_, err := auth.DisableAccess(e.Root.Dir)
			return err
		}
		result = next
		return auth.SaveAccess(e.Root.Dir, next)
	})
	if err != nil {
		return nil, err
	}
	if result == nil {
		c.Info("Removed %s and disabled browser access. Restart LocalBase to apply it.", id)
		return map[string]any{"removed": true, "config": nil, "restartRequired": true}, nil
	}
	c.Info("Removed %s %s. Restart LocalBase to apply it.", label, id)
	return map[string]any{"removed": true, "config": result.Summary(), "restartRequired": true}, nil
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

func rolesOption(c *cli.Ctx, min int) ([]string, error) {
	v, err := required(c, "roles", "roles")
	if err != nil {
		return nil, err
	}
	roles := []string{}
	if strings.TrimSpace(v) != "" {
		for _, r := range strings.Split(v, ",") {
			roles = append(roles, strings.TrimSpace(r))
		}
	}
	if len(roles) < min {
		return nil, cli.Inputf("roles: Too small: expected array to have >=%d items", min)
	}
	return roles, nil
}

// accessError turns domain errors into their messages.
func accessError(err error) error {
	var ae *auth.AccessError
	if errors.As(err, &ae) {
		return errors.New(ae.Message)
	}
	return err
}

func userCommand(name, summary, operation, verb, status string) *cli.Command {
	return &cli.Command{Name: name, Summary: summary, Options: []cli.Option{
		{Name: "email", Value: "<person@example.com>", Help: "Managed user email address"},
	}, Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
		email, err := emailOption(c, "email", "email")
		if err != nil {
			return nil, err
		}
		var user *auth.User
		warn := noManagerWarning(c, e)
		err = withOperation(e, operation, func() (err error) {
			if status == "" {
				user, err = auth.RemoveUser(context.Background(), e.Store, email)
			} else {
				user, err = auth.SetUserStatus(context.Background(), e.Store, email, status)
			}
			return err
		})
		if err != nil {
			return nil, accessError(err)
		}
		c.Info("%s %s.", verb, user.Email)
		warn()
		return map[string]any{"user": user}, nil
	})}
}

func accessCommand() *cli.Command {
	ctx := context.Background()
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
	return ordered(&cli.Command{Name: "access", Summary: "Manage browser access", Sub: []*cli.Command{
		{Name: "show", Summary: "Show browser identity provider configuration", Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
			cfg, err := auth.LoadAccess(e.Root.Dir)
			if err != nil {
				return nil, err
			}
			if cfg == nil {
				c.Info("Browser access is not configured.")
				return map[string]any{"config": nil}, nil
			}
			c.Info("Browser access: %s for %s", cfg.Provider.Kind, cfg.Origin)
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
			err = withOperation(e, "configure browser access", func() error {
				current, err := auth.LoadAccess(e.Root.Dir)
				if err != nil {
					return err
				}
				if perms == nil {
					perms = auth.DefaultBrowserPermissions
					if current != nil {
						perms = current.Permissions
					}
				}
				cfg = &auth.BrowserAccess{Provider: auth.Provider{Kind: "cloudflare-access", TeamDomain: team, Audience: audience},
					Origin: o, Permissions: perms}
				if err := cfg.Validate(); err != nil {
					return cli.Inputf("%s", err.Error())
				}
				return auth.SaveAccess(e.Root.Dir, cfg)
			})
			if err != nil {
				return nil, err
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
				cfg, err := saveRegistration(e, reg, o, perms, nil)
				if err != nil {
					return nil, err
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
				cfg, err := saveRegistration(e, auth.Registration{Kind: "magic-link", ID: id, Name: name}, o, perms,
					func(current *auth.BrowserAccess) error {
						if email, err := auth.LoadEmail(e.Root.Dir); err != nil || email == nil {
							return cli.Inputf("Configure email delivery before enabling magic-link authentication.")
						}
						if current != nil && current.Provider.Kind == "direct" {
							for _, r := range current.Provider.Registrations {
								if r.Kind == "magic-link" && r.ID != id {
									return cli.Inputf("Remove magic-link registration %s before adding another.", r.ID)
								}
							}
						}
						return nil
					})
				if err != nil {
					return nil, err
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
				c.Info("Disabled browser access. Restart LocalBase to apply it.")
			} else {
				c.Info("Browser access was already disabled. Restart LocalBase to apply it.")
			}
			return map[string]any{"disabled": disabled, "restartRequired": disabled}, nil
		})},
		{Name: "email", Summary: "Manage email delivery", Sub: []*cli.Command{
			{Name: "show", Summary: "Show email delivery configuration", Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
				cfg, err := auth.LoadEmail(e.Root.Dir)
				if err != nil {
					return nil, err
				}
				if cfg == nil {
					c.Info("Email delivery is not configured.")
					return map[string]any{"config": nil}, nil
				}
				c.Info("Email delivery: %s:%d from %s", cfg.Host, cfg.Port, cfg.From)
				return map[string]any{"config": cfg.Summary()}, nil
			})},
			{Name: "configure", Summary: "Configure SMTP email delivery", Options: []cli.Option{
				{Name: "host", Value: "<smtp.example.com>", Help: "SMTP server hostname"},
				{Name: "port", Value: "<587>", Help: "SMTP server port"},
				{Name: "security", Value: "<tls|starttls>", Help: "SMTP transport security"},
				{Name: "username", Value: "<username>", Help: "SMTP username"},
				{Name: "password-env", Value: "<ENVIRONMENT_VARIABLE>", Help: "Environment variable containing the SMTP password"},
				{Name: "from", Value: "<localbase@example.com>", Help: "Sender email address"},
			}, Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
				host, err := required(c, "host", "host")
				if err != nil {
					return nil, err
				}
				port, given, perr := c.Int("port")
				if !given {
					return nil, cli.Inputf("port: Invalid input: expected number, received NaN")
				}
				if perr != nil || port < 1 || port > 65535 {
					return nil, cli.Inputf("port: Invalid port")
				}
				security, err := required(c, "security", "security")
				if err != nil {
					return nil, err
				}
				if security != "tls" && security != "starttls" {
					return nil, cli.Inputf(`security: Invalid option: expected one of "tls"|"starttls"`)
				}
				from, err := emailOption(c, "from", "from")
				if err != nil {
					return nil, err
				}
				passwordEnv := c.String("password-env")
				if c.Has("password-env") && !envNameRe.MatchString(passwordEnv) {
					return nil, cli.Inputf("passwordEnv: Invalid string: must match pattern /^[A-Za-z_][A-Za-z0-9_]*$/")
				}
				if (c.String("username") != "") != (passwordEnv != "") {
					return nil, cli.Inputf("input: Provide both --username and --password-env, or neither.")
				}
				authn := auth.EmailAuth{Kind: "none"}
				if passwordEnv != "" {
					password := os.Getenv(passwordEnv)
					if password == "" {
						return nil, cli.Inputf("SMTP password environment variable %s is empty or unavailable.", passwordEnv)
					}
					authn = auth.EmailAuth{Kind: "password", Username: c.String("username"), Password: password}
				}
				cfg := &auth.EmailConfig{Host: host, Port: port, Security: security, Authentication: authn, From: from}
				if err := cfg.Validate(); err != nil {
					return nil, cli.Inputf("%s", err.Error())
				}
				if err := withOperation(e, "configure email delivery", func() error { return auth.SaveEmail(e.Root.Dir, cfg) }); err != nil {
					return nil, err
				}
				c.Info("Saved email delivery configuration.")
				return map[string]any{"config": cfg.Summary(), "restartRequired": false}, nil
			})},
			{Name: "test", Summary: "Send a test email", Options: []cli.Option{
				{Name: "to", Value: "<person@example.com>", Help: "Test recipient"},
			}, Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
				to, err := emailOption(c, "to", "to")
				if err != nil {
					return nil, err
				}
				cfg, err := auth.LoadEmail(e.Root.Dir)
				if err != nil {
					return nil, err
				}
				if cfg == nil {
					return nil, cli.Inputf("Configure email delivery before testing it.")
				}
				if err := auth.SendEmail(cfg, to, "LocalBase email delivery test", "LocalBase successfully sent this test email."); err != nil {
					return nil, err
				}
				c.Info("Sent a test email to %s.", to)
				return map[string]any{"delivered": true, "to": to}, nil
			})},
			{Name: "disable", Summary: "Disable email delivery", Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
				var disabled bool
				err := withOperation(e, "disable email delivery", func() (err error) {
					current, err := auth.LoadAccess(e.Root.Dir)
					if err != nil {
						return err
					}
					if current != nil && current.Provider.Kind == "direct" {
						for _, r := range current.Provider.Registrations {
							if r.Kind == "magic-link" {
								return cli.Inputf("Remove the magic-link registration before disabling email delivery.")
							}
						}
					}
					disabled, err = auth.DisableEmail(e.Root.Dir)
					return err
				})
				if err != nil {
					return nil, err
				}
				if disabled {
					c.Info("Disabled email delivery.")
				} else {
					c.Info("Email delivery was already disabled.")
				}
				return map[string]any{"disabled": disabled, "restartRequired": false}, nil
			})},
		}},
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
				check := policy
				if err := check.Normalize(); err != nil {
					return nil, cli.Inputf("Invalid access policy: %s", err.Error())
				}
				warn := noManagerWarning(c, e)
				err = withOperation(e, "configure browser access", func() error {
					if cfg, err := auth.LoadAccess(e.Root.Dir); err != nil {
						return err
					} else if cfg == nil {
						return cli.Inputf("Configure a browser identity provider first.")
					}
					return auth.ApplyPolicy(ctx, e.Store, &policy)
				})
				if err != nil {
					return nil, accessError(err)
				}
				c.Info("Saved browser access policy. Changes apply immediately.")
				warn()
				return map[string]any{"policy": &policy, "restartRequired": false}, nil
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
					c.Info("Cleared browser access policy. Provider-wide permissions now apply.")
					warn()
				} else {
					c.Info("Browser access policy was already clear.")
				}
				return map[string]any{"cleared": cleared, "restartRequired": false}, nil
			})},
		}},
		{Name: "users", Summary: "Provision and manage browser users", Sub: []*cli.Command{
			{Name: "list", Summary: "List provisioned browser users", Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
				users, err := auth.ListUsers(ctx, e.Store)
				if err != nil {
					return nil, err
				}
				c.Info("%d managed users.", len(users))
				return map[string]any{"users": users}, nil
			})},
			{Name: "invite", Summary: "Provision a browser user by verified email", Options: []cli.Option{
				{Name: "email", Value: "<person@example.com>", Help: "Verified email address"},
				{Name: "roles", Value: "<role,...>", Help: "Named roles to grant after first sign-in"},
				{Name: "send-email", Bool: true, Help: "Send the invitation using configured email delivery"},
			}, Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
				email, err := emailOption(c, "email", "email")
				if err != nil {
					return nil, err
				}
				roles, err := rolesOption(c, 1)
				if err != nil {
					return nil, err
				}
				var user *auth.User
				var signIn string
				var delivery *auth.EmailConfig
				err = withOperation(e, "provision a browser user", func() error {
					cfg, err := auth.LoadAccess(e.Root.Dir)
					if err != nil {
						return err
					}
					if cfg == nil {
						return cli.Inputf("Configure a browser identity provider first.")
					}
					if c.Bool("send-email") {
						if delivery, err = auth.LoadEmail(e.Root.Dir); err != nil {
							return err
						} else if delivery == nil {
							return cli.Inputf("Configure email delivery before sending an invitation email.")
						}
					}
					user, err = auth.InviteUser(ctx, e.Store, email, roles)
					signIn = cfg.Origin + "/app/login"
					return err
				})
				if err != nil {
					return nil, accessError(err)
				}
				delivered := delivery != nil && auth.SendEmail(delivery, user.Email, "You were invited to LocalBase",
					"You were invited to LocalBase. Sign in here:\n\n"+signIn) == nil
				if delivered {
					c.Info("Invited %s and sent an email.", user.Email)
				} else {
					c.Info("Invited %s. Sign in at %s", user.Email, signIn)
				}
				return map[string]any{"user": user, "signInUrl": signIn, "emailDelivered": delivered}, nil
			})},
			{Name: "roles", Summary: "Replace a browser user's named roles", Options: []cli.Option{
				{Name: "email", Value: "<person@example.com>", Help: "Managed user email address"},
				{Name: "roles", Value: "<role,...>", Help: "Complete named role set, or empty to grant none"},
			}, Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
				email, err := emailOption(c, "email", "email")
				if err != nil {
					return nil, err
				}
				roles, err := rolesOption(c, 0)
				if err != nil {
					return nil, err
				}
				var user *auth.User
				warn := noManagerWarning(c, e)
				err = withOperation(e, "replace browser user roles", func() (err error) {
					user, err = auth.ReplaceUserRoles(ctx, e.Store, email, roles)
					return err
				})
				if err != nil {
					return nil, accessError(err)
				}
				c.Info("Replaced roles for %s.", user.Email)
				warn()
				return map[string]any{"user": user}, nil
			})},
			userCommand("enable", "Enable a browser user", "enable a browser user", "Enabled", "active"),
			userCommand("disable", "Disable a browser user and revoke all browser permissions", "disable a browser user", "Disabled", "disabled"),
			userCommand("remove", "Remove a browser user and linked identities", "remove a browser user", "Removed", ""),
		}},
	}}, "show", "cloudflare", "github", "magic-link", "email", "oidc", "disable", "policy", "users")
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

// strictJSON decodes rejecting unknown fields.
func strictJSON(b []byte, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s", strings.TrimPrefix(err.Error(), "json: "))
	}
	return nil
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
	var perms []string
	if cfg, err := auth.LoadAccess(e.Root.Dir); err == nil && cfg != nil {
		perms = cfg.Permissions
	}
	g, err := auth.LoadGuard(context.Background(), e.Store, perms)
	return err == nil && g.HasManager()
}
