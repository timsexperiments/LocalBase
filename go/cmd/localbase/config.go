package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/audit"
	"github.com/timsexperiments/LocalBase/go/internal/backup"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/conf"
	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
	"github.com/timsexperiments/LocalBase/go/internal/lease"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/service"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// `config validate|plan|apply|show`: the declarative TOML document (TS
// domains/config/commands/config.ts). None of them initializes a root:
// plan and show read the database read-only, and apply creates the root
// only when it saves.

func configCommand() *cli.Command {
	file := cli.Option{Name: "file", Value: "<path|->", Help: "Versioned TOML file, or - for stdin"}
	return &cli.Command{Name: "config", Summary: "Validate, plan, apply, and show declarative configuration", Sub: []*cli.Command{
		{Name: "validate", Summary: "Validate a desired configuration without changing state", Options: []cli.Option{file},
			InputExitCode: 1, Run: runConfigValidate},
		{Name: "plan", Summary: "Compare desired configuration with persisted settings", InputExitCode: 1,
			Options: []cli.Option{file, {Name: "detailed-exit-code", Bool: true, Help: "Exit 2 for changes, 0 for no changes, 1 for errors"}},
			Run:     runConfigPlan, ExitCodeFor: func(data any) int { return data.(*planResult).exit }},
		{Name: "apply", Summary: "Atomically apply desired configuration", InputExitCode: 1, Options: []cli.Option{file,
			{Name: "restart", Value: "<auto|always|never>", Help: "Restart policy (default: auto)"},
			{Name: "wait", Bool: true, Help: "Wait up to 30 seconds for gateway readiness"}},
			Run: runConfigApply},
		{Name: "show", Summary: "Emit a re-applicable TOML document with secrets omitted", InputExitCode: 1, Run: runConfigShow},
	}}
}

// configRoot is the canonical root, which need not exist.
func configRoot(c *cli.Ctx) (string, error) {
	return root.Canonical(root.Resolve(c.Global.Root))
}

// readDocument reads --file (or stdin) and validates it against the
// catalog the root sees.
func readDocument(c *cli.Ctx, dir string) (*store.Config, *catalog.Catalog, error) {
	if !c.Has("file") || c.String("file") == "" {
		return nil, nil, cli.Inputf("file: Invalid input: expected string, received undefined")
	}
	var b []byte
	var err error
	if path := c.String("file"); path == "-" {
		b, err = io.ReadAll(os.Stdin)
	} else {
		b, err = os.ReadFile(path)
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("ENOENT: no such file or directory, open '%s'", c.String("file"))
	}
	if err != nil {
		return nil, nil, err
	}
	cat, err := catalog.LoadRoot(dir, nil)
	if err != nil {
		return nil, nil, err
	}
	desired, err := conf.ParseDocument(string(b), cat)
	if err != nil {
		return nil, nil, inputError(err)
	}
	return desired, cat, nil
}

func inputError(err error) error {
	var de *conf.DocumentError
	if errors.As(err, &de) {
		return cli.Inputf("%s", de.Message)
	}
	return err
}

func invalidConfiguration(dir, detail string) error {
	return fmt.Errorf("Invalid LocalBase configuration in %s/local-base.db: %s. Repair the invalid row or run \"local-base reset --root %s --yes\" to recreate the database.",
		dir, detail, dir)
}

// readConfigIfPresent is the saved configuration, nil without a database,
// and whether a restart is pending.
func readConfigIfPresent(dir string, cat *catalog.Catalog) (*store.Config, bool, error) {
	path := dir + "/local-base.db"
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, &unreadableSettings{Root: dir, Err: err} // there, but not to be read: not a fresh root
	}
	if msg := backup.UnfinishedWrite(context.Background(), dir); msg != "" {
		return nil, false, errors.New(msg)
	}
	st, err := store.OpenReadOnly(path)
	if err != nil {
		return nil, false, err
	}
	defer st.Close()
	ctx := context.Background()
	cfg, err := st.LoadConfig(ctx)
	if errors.Is(err, store.ErrNotConfigured) {
		return nil, false, invalidConfiguration(dir, "configuration row is missing")
	}
	if err != nil {
		return nil, false, err
	}
	conf.KeepFormerSelections(dir, cfg, cat)
	if issues := conf.ValidateModels(cfg, cat, false); len(issues) > 0 {
		return nil, false, invalidConfiguration(dir, conf.Summary(issues))
	}
	pending, err := st.RestartPending(ctx)
	return cfg, pending, err
}

func printPlan(c *cli.Ctx, p *conf.Plan) {
	if !p.Changed {
		c.Info("No configuration changes.")
	}
	if p.PendingRestart {
		c.Info("Pending restart: saved static settings await managed startup.")
	}
	for _, ch := range p.Changes {
		c.Info("%s: %s -> %s [%s]", ch.Path, jsjson.Stringify(ch.Before), jsjson.Stringify(ch.After), ch.Activation)
	}
}

func runConfigValidate(c *cli.Ctx) (any, error) {
	dir, err := configRoot(c)
	if err != nil {
		return nil, err
	}
	desired, _, err := readDocument(c, dir)
	if err != nil {
		return nil, err
	}
	c.Info("Configuration is valid (version 1).")
	return map[string]any{"valid": true, "configuration": conf.Document(desired)}, nil
}

type planResult struct {
	*conf.Plan
	exit int
}

func runConfigPlan(c *cli.Ctx) (any, error) {
	dir, err := configRoot(c)
	if err != nil {
		return nil, err
	}
	desired, cat, err := readDocument(c, dir)
	if err != nil {
		return nil, err
	}
	current, pending, err := readConfigIfPresent(dir, cat)
	if err != nil {
		return nil, err
	}
	plan, err := conf.PlanChanges(current, desired, pending, cat)
	if err != nil {
		return nil, inputError(err)
	}
	printPlan(c, plan)
	res := &planResult{Plan: plan}
	if c.Bool("detailed-exit-code") && (plan.Changed || plan.PendingRestart) {
		res.exit = 2
	}
	return res, nil
}

type applyResult struct {
	*conf.Plan
	audit.Outcome
	Activation string `json:"activation"`
	Readiness  string `json:"readiness"`
}

func runConfigApply(c *cli.Ctx) (any, error) {
	policy := "auto"
	if c.Has("restart") {
		policy = c.String("restart")
		if policy != "auto" && policy != "always" && policy != "never" {
			return nil, cli.Inputf(`restart: Invalid option: expected one of "auto"|"always"|"never"`)
		}
	}
	wait := c.Bool("wait")
	dir, err := configRoot(c)
	if err != nil {
		return nil, err
	}
	desired, cat, err := readDocument(c, dir)
	if err != nil {
		return nil, err
	}
	lease, err := lease.AcquireOperation(dir, "service-start")
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	dbExists := false
	if _, err := os.Stat(dir + "/local-base.db"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, &unreadableSettings{Root: dir, Err: err} // there, but not to be read: not a fresh root
	} else if err == nil {
		dbExists = true
		if _, err := root.Open(dir); err != nil {
			return nil, err
		}
		st, err := store.Open(dir + "/local-base.db") // applies pending migrations
		if err != nil {
			return nil, err
		}
		st.Close()
	}
	var audited *audit.Log
	current, pending, err := readConfigIfPresent(dir, cat)
	if err != nil {
		return nil, err
	}
	plan, err := conf.PlanChanges(current, desired, pending, cat)
	if err != nil {
		return nil, inputError(err)
	}
	base := current
	if base == nil {
		base = conf.Default(dir, 0, cat)
	}
	next := conf.Compose(base, desired)

	restart := policy == "always"
	var r *root.Root
	if restart || (plan.RestartRequired && policy == "auto") || wait {
		r = &root.Root{Dir: dir}
		in, err := service.Inspect(r, false)
		if err != nil {
			return nil, err
		}
		if restart, err = RestartDecision(policy, wait, plan.RestartRequired, in.Service.State, in.Service.ManagerAvailable); err != nil {
			return nil, inputError(err)
		}
	}
	ctx := context.Background()
	withStore := func(fn func(st *store.Store) error) error {
		rr, err := root.Init(dir)
		if err != nil {
			return err
		}
		st, err := store.Open(rr.DB())
		if err != nil {
			return err
		}
		defer st.Close()
		return fn(st)
	}
	if plan.Changed {
		if !dbExists {
			if issues := conf.ValidateModels(next, cat, true); len(issues) > 0 {
				return nil, cli.Inputf("%s", conf.Summary(issues))
			}
		}
		if issues := conf.ValidateRow(next); len(issues) > 0 {
			return nil, &conf.InvalidError{Root: dir, Issues: issues}
		}
		if err := withStore(func(st *store.Store) error {
			for _, k := range store.Kinds {
				if err := os.MkdirAll(dir+"/models/"+k, 0o700); err != nil {
					return err
				}
			}
			return st.SaveConfigMarking(ctx, next)
		}); err != nil {
			return nil, err
		}
		if r, err := root.Open(dir); err == nil {
			audited = audit.New(r, c.Stderr)
			audited.Change("config", "apply", "configuration", "")
		}
	}
	if restart {
		if err := withStore(func(st *store.Store) error { return st.MarkRestartPending(ctx, next) }); err != nil {
			return nil, err
		}
	}
	activate := func() error {
		if restart {
			if r == nil {
				r = &root.Root{Dir: dir}
			}
			if _, err := service.RestartWithin(r, lease); err != nil {
				return err
			}
		}
		if wait {
			return service.WaitReady(r, 30*time.Second)
		}
		return nil
	}
	if err := activate(); err != nil {
		var markErr error
		if restart {
			markErr = withStore(func(st *store.Store) error { return st.MarkRestartPendingUnlessChanged(ctx, next) })
		}
		return nil, activationFailed(err, markErr)
	}
	if _, plan.PendingRestart, err = readConfigIfPresent(dir, cat); err != nil {
		return nil, err
	}
	res := &applyResult{Plan: plan, Readiness: "not-checked"}
	switch {
	case restart:
		res.Activation = "restarted"
	case plan.RestartRequired:
		res.Activation = "restart-required"
	case plan.Changed:
		res.Activation = "hot"
	default:
		res.Activation = "unchanged"
	}
	if wait {
		res.Readiness = "ready"
	}
	if audited != nil {
		res.Outcome = audited.Outcome()
	}
	printPlan(c, plan)
	c.Info("Activation: %s. Readiness: %s.", res.Activation, res.Readiness)
	return res, nil
}

func runConfigShow(c *cli.Ctx) (any, error) {
	dir, err := configRoot(c)
	if err != nil {
		return nil, err
	}
	cat, err := catalog.LoadRoot(dir, nil)
	if err != nil {
		return nil, err
	}
	cfg, pending, err := readConfigIfPresent(dir, cat)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, invalidConfiguration(dir, "configuration database is missing")
	}
	document := conf.Render(cfg)
	if !c.Global.JSON {
		c.Info("%s", cli.Lines(strings.TrimRight(document, "\n")))
		if pending {
			c.Error("Pending restart: saved static settings await managed startup.")
		}
	}
	return map[string]any{"document": document, "pendingRestart": pending}, nil
}

// RestartDecision is whether apply restarts the managed service, or why it
// refuses before saving (TS applyConfiguration), with a *conf.DocumentError (bad input): auto restarts for
// restart-required changes unless the gateway is foreground, unknown or
// stopping; always restarts; --wait needs a running gateway or a restart.
func RestartDecision(policy string, wait, restartRequired bool, state string, managerAvailable bool) (bool, error) {
	restart := policy == "always"
	running := state == "running" || state == "starting"
	unsafe := state == "foreground" || state == "unknown" || state == "stopping"
	if policy == "auto" && restartRequired {
		if unsafe {
			return false, &conf.DocumentError{Message: "Cannot automatically restart this gateway. Use --restart never to save, then restart its owner explicitly."}
		}
		restart = true
	}
	if restart && (!managerAvailable || unsafe) {
		return false, &conf.DocumentError{Message: "Cannot safely restart this gateway with the service manager."}
	}
	if wait && !restart && (restartRequired || (!running && state != "foreground")) {
		return false, &conf.DocumentError{Message: "--wait requires a running gateway without pending restart changes, or --restart always."}
	}
	return restart, nil
}

// activationFailed says what is owed. If the pending restart couldn't be recorded, status won't show
// it, so this has to.
func activationFailed(err, markErr error) error {
	if markErr != nil {
		return fmt.Errorf("Configuration is saved, but activation failed: %s. A restart is needed, and that couldn't be recorded either (%s), so local-base status won't show it. Run local-base restart.",
			err.Error(), markErr.Error())
	}
	return fmt.Errorf("Configuration is saved, but activation failed: %s. Retry local-base restart, or config apply --restart always --wait.", err.Error())
}
