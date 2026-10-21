package main

import (
	"context"
	"errors"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/conf"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

func initCommand() *cli.Command {
	return &cli.Command{Name: "init", Summary: "Initialize the LocalBase data directory", Run: func(c *cli.Ctx) (any, error) {
		e, err := openEnv(c)
		if err != nil {
			return nil, err
		}
		defer e.Close()
		cfg := conf.Default(e.Root.Dir, e.memoryGB(), e.Catalog)
		if err := e.save(cfg); err != nil {
			return nil, err
		}
		c.Info("Initialized local-base at %s", cfg.Root)
		c.Info("LLM directory: %s", e.Root.ModelsDir("llm"))
		c.Info("STT directory: %s", e.Root.ModelsDir("stt"))
		return map[string]any{"configuration": conf.Public(cfg)}, nil
	}}
}

// publicKey is the key metadata commands print (never the hash).
func publicKey(k *store.APIKey) map[string]any {
	scopes := k.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	m := map[string]any{"id": k.ID, "name": k.Name, "prefix": k.Prefix, "scopes": scopes,
		"createdAt": k.CreatedAt, "lastRotatedAt": k.LastRotatedAt}
	if k.ExpiresAt != nil {
		m["expiresAt"] = *k.ExpiresAt
	}
	if k.RevokedAt != nil {
		m["revokedAt"] = *k.RevokedAt
	}
	return m
}

// parseScopes reads a comma-separated permission list; empty grants none.
// The result is in canonical order.
func parseScopes(v string) ([]string, error) {
	list := []string{}
	if strings.TrimSpace(v) == "" {
		return list, nil
	}
	for _, s := range strings.Split(v, ",") {
		s = strings.TrimSpace(s)
		if !contains(auth.Permissions, s) {
			return nil, cli.Inputf("scopes: Invalid option: expected one of %s", `"`+strings.Join(auth.Permissions, `"|"`)+`"`)
		}
		list = append(list, s)
	}
	return auth.Canonical(list), nil
}

func keyNotFound(err error) error {
	var nf *store.ErrKeyNotFound
	if errors.As(err, &nf) {
		return errors.New(nf.Error())
	}
	return err
}

// withEnv opens the root for a command.
func withEnv(run func(c *cli.Ctx, e *env) (any, error)) func(c *cli.Ctx) (any, error) {
	return func(c *cli.Ctx) (any, error) {
		e, err := openEnv(c)
		if err != nil {
			return nil, err
		}
		defer e.Close()
		return run(c, e)
	}
}

func keysCommand() *cli.Command {
	ctx := context.Background()
	return &cli.Command{Name: "keys", Summary: "Manage API keys", Sub: []*cli.Command{
		{Name: "list", Summary: "List API keys", Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
			keys, err := e.Store.ListAPIKeys(ctx)
			if err != nil {
				return nil, err
			}
			out := []map[string]any{}
			if len(keys) == 0 {
				c.Info("No API keys found. Create one with: local-base keys create --name default")
			}
			for i := range keys {
				k := &keys[i]
				line := k.ID + " | " + k.Name + " | prefix=" + k.Prefix + " | scopes=" + strings.Join(k.Scopes, ",") +
					" | created=" + k.CreatedAt + " | rotated=" + k.LastRotatedAt
				if k.ExpiresAt != nil {
					line += " | expires=" + *k.ExpiresAt
				}
				if k.RevokedAt != nil {
					line += " | revoked=" + *k.RevokedAt
				}
				c.Info("%s", line)
				out = append(out, publicKey(k))
			}
			return map[string]any{"keys": out}, nil
		})},
		{Name: "create", Summary: "Create an API key", Options: []cli.Option{
			{Name: "name", Value: "<label>", Help: "Key label"},
			{Name: "scopes", Value: "<permission,...>", Help: "Comma-separated permissions; defaults to inference and models:read. Empty grants none"},
			{Name: "expires-days", Value: "<days>", Help: "Expiration period"},
		}, Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
			name := "manual"
			if c.Has("name") {
				if name = c.String("name"); name == "" {
					return nil, cli.Inputf("name: Too small: expected string to have >=1 characters")
				}
			}
			scopes := auth.DefaultKeyScopes
			if c.Has("scopes") {
				var err error
				if scopes, err = parseScopes(c.String("scopes")); err != nil {
					return nil, err
				}
			}
			days, given, err := c.Int("expires-days")
			if err != nil || (given && days <= 0) {
				return nil, cli.Inputf("expiresDays: must be a positive integer")
			}
			k, raw, err := e.Store.CreateAPIKeyExpiring(ctx, name, scopes, days)
			if err != nil {
				return nil, err
			}
			c.Info("Created key id=%s name=%s prefix=%s", k.ID, k.Name, k.Prefix)
			if !c.Global.JSON {
				c.Info("secret=%s", raw)
				c.Info("Store this secret now. It is not shown again.")
			}
			return map[string]any{"key": publicKey(k), "secret": raw}, nil
		})},
		{Name: "revoke", Summary: "Revoke an API key", Usage: "<keyId>", MinArgs: 1, MaxArgs: 1,
			Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
				k, err := e.Store.RevokeAPIKey(ctx, c.Args[0])
				if err != nil {
					return nil, keyNotFound(err)
				}
				c.Info("Revoked key %s (%s)", k.ID, k.Name)
				return map[string]any{"key": publicKey(k)}, nil
			})},
		{Name: "rotate", Summary: "Rotate an API key", Usage: "<keyId>", MinArgs: 1, MaxArgs: 1,
			Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
				k, raw, err := e.Store.RotateAPIKey(ctx, c.Args[0])
				if err != nil {
					return nil, keyNotFound(err)
				}
				c.Info("Rotated key %s (%s)", k.ID, k.Name)
				if !c.Global.JSON {
					c.Info("new_secret=%s", raw)
					c.Info("Store this secret now. It is not shown again.")
				}
				return map[string]any{"key": publicKey(k), "secret": raw}, nil
			})},
		{Name: "scopes", Summary: "Replace an API key's scopes", Usage: "<keyId>", MinArgs: 1, MaxArgs: 1, Options: []cli.Option{
			{Name: "scopes", Value: "<permission,...>", Help: "Complete comma-separated permission set; empty grants none"},
		}, Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
			if !c.Has("scopes") {
				return nil, cli.Inputf("scopes: Invalid input: expected string, received undefined")
			}
			scopes, err := parseScopes(c.String("scopes"))
			if err != nil {
				return nil, err
			}
			k, err := e.Store.SetAPIKeyScopes(ctx, c.Args[0], scopes)
			if err != nil {
				return nil, keyNotFound(err)
			}
			c.Info("Updated scopes for key %s: %s", k.ID, strings.Join(k.Scopes, ","))
			return map[string]any{"key": publicKey(k)}, nil
		})},
	}}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
