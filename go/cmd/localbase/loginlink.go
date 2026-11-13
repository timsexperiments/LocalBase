package main

import (
	"context"
	"errors"
	"github.com/timsexperiments/LocalBase/go/internal/service"
	"net"
	"strconv"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
)

// gatewayListen is the running gateway's address, else the configured one.
func gatewayListen(e *env) (addr string, running bool) {
	if state, owner, _ := service.VerifiedOwnerState(e.Root); state == "active" && owner != nil && owner.Port > 0 {
		return net.JoinHostPort(owner.Host, strconv.Itoa(owner.Port)), true
	}
	return net.JoinHostPort(e.Config.GatewayHost, strconv.Itoa(e.Config.GatewayPort)), false
}

func issueLoginLink(e *env, addr string, forced bool) (string, error) {
	link, id, err := auth.LocalLinkURL(context.Background(), e.Store, addr)
	if err != nil {
		return "", err
	}
	e.audit.LocalLinkIssued(id, forced)
	return link, nil
}

// firstRunLinkAllowed: a service-managed gateway never prints one, since its
// captured stderr must never hold a link.
func firstRunLinkAllowed(noAuth, browserEnabled bool, serviceToken string) bool {
	return !noAuth && !browserEnabled && serviceToken == ""
}

// firstRunLoginLink issues the link shown once on a root where browser access is
// not configured (auth.FirstRunLink); "" when it was already shown or none can be used here.
func firstRunLoginLink(e *env, addr string) string {
	link, id, err := auth.FirstRunLink(context.Background(), e.Store, addr)
	if id != "" {
		e.audit.LocalLinkIssued(id, false)
	}
	if err != nil {
		return ""
	}
	return link
}

func printLoginLink(c *cli.Ctx, link string) {
	if link == "" {
		return
	}
	c.Info("\nSign in from a browser on this computer. This link works once, within %d minutes:\n\n  %s\n", int(auth.LocalLinkTTL.Minutes()), link)
	c.Info("It does not work from a phone or another computer; those need an identity provider: local-base access --help")
}

func loginLinkCommand() *cli.Command {
	return &cli.Command{Name: "login-link", Summary: "Print a one-time browser sign-in link for this computer", Options: []cli.Option{
		{Name: "force", Bool: true, Help: "Issue one even though an identity provider is configured"},
		{Name: "revoke", Bool: true, Help: "End every local session and unused link instead"},
	}, Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
		ctx := context.Background()
		if c.Bool("revoke") {
			n, err := auth.RevokeAllLocal(ctx, e.Store)
			if err != nil {
				return nil, err
			}
			e.audit.LocalSessionsRevoked(n)
			c.Info("Ended %d local sessions and any unused link.", n)
			return map[string]any{"revoked": n}, nil
		}
		access, err := auth.LoadAccess(e.Root.Dir)
		if err != nil {
			return nil, err
		}
		forced := c.Bool("force")
		if access != nil && !forced {
			return nil, cli.Inputf("An identity provider is configured, so sign in through it. A local link grants every permission, without the provider or its policy; pass --force if you mean that.")
		}
		addr, running := gatewayListen(e)
		link, err := issueLoginLink(e, addr, forced)
		if err != nil {
			return nil, err
		}
		printLoginLink(c, link)
		if !running {
			c.Info("The gateway is not running. Start it first: local-base start")
		}
		return map[string]any{"url": link, "expiresInSeconds": int(auth.LocalLinkTTL.Seconds()), "gatewayRunning": running}, nil
	})}
}

func localSessionsCommand() *cli.Command {
	return &cli.Command{Name: "sessions", Summary: "List and end local browser sessions", Sub: []*cli.Command{
		{Name: "list", Summary: "List local browser sessions", Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
			list, err := auth.ListLocalSessions(context.Background(), e.Store)
			if err != nil {
				return nil, err
			}
			if len(list) == 0 {
				c.Info("No local sessions.")
			}
			for _, s := range list {
				c.Info("%s  last used %s  expires %s", s.ID, time.UnixMilli(s.LastSeenAt).Format(time.RFC3339), time.UnixMilli(s.ExpiresAt).Format(time.RFC3339))
			}
			return map[string]any{"sessions": list}, nil
		})},
		{Name: "revoke", Summary: "End one local browser session, or every session", Usage: "[<id>]", MaxArgs: 1, IDArg: true,
			Options: []cli.Option{{Name: "all", Bool: true, Help: "End every browser session, local or through a provider, and unused sign-in links"}},
			Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
				if c.Bool("all") == (len(c.Args) == 1) {
					return nil, cli.Inputf("Give a session id, or --all.")
				}
				if c.Bool("all") {
					n, err := auth.RevokeAllSessions(context.Background(), e.Store)
					if err != nil {
						return nil, err
					}
					e.audit.SessionsRevoked(n)
					c.Info("Ended %d sessions.", n)
					return map[string]any{"revoked": n}, nil
				}
				found, err := auth.RevokeLocalSession(context.Background(), e.Store, c.Args[0])
				if err != nil {
					return nil, err
				}
				if !found {
					return nil, errors.New("No local session with that id.")
				}
				e.audit.LocalSessionRevoked(c.Args[0])
				c.Info("Ended the session.")
				return map[string]any{"revoked": 1}, nil
			})},
	}}
}
