package main

import (
	"os"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/termsafe"
)

// accessEmailCommands is the access command for email delivery.
func accessEmailCommands() []*cli.Command {
	return []*cli.Command{
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
				c.Info("Email delivery: %s:%d from %s", termsafe.PrintableASCII(cfg.Host), cfg.Port, termsafe.PrintableASCII(cfg.From))
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
				// The current configuration is read only to say whether this changes it: one that
				// can't be read counts as changed, and is replaced.
				changed := true
				if err := withOperation(e, "configure email delivery", func() error {
					if previous, err := auth.LoadEmail(e.Root.Dir); err == nil {
						changed = auth.EmailChanged(previous, cfg)
					}
					return auth.SaveEmail(e.Root.Dir, cfg)
				}); err != nil {
					return nil, err
				}
				if changed {
					e.audit.Change("access", "configure-email-delivery", "email-delivery", "smtp")
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
				if err := auth.SendTestEmail(cfg, to); err != nil {
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
					if auth.CheckDisableEmail(current) != nil {
						return cli.Inputf("Remove the magic-link registration before disabling email delivery.")
					}
					disabled, err = auth.DisableEmail(e.Root.Dir)
					return err
				})
				if err != nil {
					return nil, err
				}
				if disabled {
					e.audit.Change("access", "disable-email-delivery", "email-delivery", "smtp")
					c.Info("Disabled email delivery.")
				} else {
					c.Info("Email delivery was already disabled.")
				}
				return map[string]any{"disabled": disabled, "restartRequired": false}, nil
			})},
		}},
	}
}
