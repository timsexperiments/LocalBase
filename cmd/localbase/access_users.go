package main

import (
	"context"
	"errors"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/termsafe"
)

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

func userCommand(name, summary, operation, verb, status string) *cli.Command {
	return &cli.Command{Name: name, Summary: summary, Options: []cli.Option{
		{Name: "email", Value: "<person@example.com>", Help: "Managed user email address"},
	}, Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
		email, err := emailOption(c, "email", "email")
		if err != nil {
			return nil, err
		}
		var user *auth.User
		changed := true // removing always changes something: a removed person isn't found
		warn := noManagerWarning(c, e)
		err = withOperation(e, operation, func() (err error) {
			if status == "" {
				user, err = auth.RemoveUser(context.Background(), e.Store, email)
			} else {
				user, changed, err = auth.SetUserStatus(context.Background(), e.Store, email, status)
			}
			return err
		})
		if err != nil {
			return nil, accessError(err)
		}
		if changed {
			e.audit.Change("access", userAction(status), "user", user.ID)
		}
		c.Info("%s %s.", verb, user.Email)
		warn()
		return map[string]any{"user": user}, nil
	})}
}

// accessUserCommands are the access commands for browser users and local sign-in: users, login-link and sessions.
func accessUserCommands() []*cli.Command {
	ctx := context.Background()
	return []*cli.Command{
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
				{Name: "issuer", Value: "<https://identity.example.com>", Help: "The sign-in provider this person will use; needed when several are configured"},
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
						// None is bad input (exit 2); a file that can't be read is the machine's
						// state, said in LoadEmail's words (exit 1).
						if delivery, err = auth.RequireEmail(e.Root.Dir); errors.Is(err, auth.ErrEmailNotConfigured) {
							return cli.Inputf("Configure email delivery before sending an invitation email.")
						} else if err != nil {
							return err
						}
					}
					issuer, err := auth.InviteIssuer(c.String("issuer"), email, cfg.Issuers(), nil)
					if err != nil {
						return issuerInput(err)
					}
					user, err = auth.InviteUser(ctx, e.Store, email, roles, issuer)
					signIn = auth.InvitationSignIn(cfg)
					return err
				})
				if err != nil {
					return nil, accessError(err)
				}
				e.audit.Change("access", "invite-user", "user", user.ID)
				delivered := delivery != nil && auth.SendInvitation(delivery, user.Email, signIn) == nil
				switch {
				case delivered:
					c.Info("Invited %s and sent an email.", user.Email)
				case delivery != nil: // asked for, and not sent: say so, without the server's words
					c.Error("Invited %s, but the email could not be sent. Sign in at %s", termsafe.PrintableASCII(user.Email), termsafe.PrintableASCII(signIn))
				default:
					c.Info("Invited %s. Sign in at %s", termsafe.PrintableASCII(user.Email), termsafe.PrintableASCII(signIn))
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
				var changed bool
				warn := noManagerWarning(c, e)
				err = withOperation(e, "replace browser user roles", func() (err error) {
					user, changed, err = auth.ReplaceUserRoles(ctx, e.Store, email, roles)
					return err
				})
				if err != nil {
					return nil, accessError(err)
				}
				if changed {
					e.audit.Change("access", "replace-user-roles", "user", user.ID)
				}
				c.Info("Replaced roles for %s.", user.Email)
				warn()
				return map[string]any{"user": user}, nil
			})},
			userCommand("enable", "Enable a browser user", "enable a browser user", "Enabled", "active"),
			userCommand("disable", "Disable a browser user and revoke all browser permissions", "disable a browser user", "Disabled", "disabled"),
			userCommand("remove", "Remove a browser user and linked identities", "remove a browser user", "Removed", ""),
		}},
		loginLinkCommand(),
		localSessionsCommand(),
	}
}
