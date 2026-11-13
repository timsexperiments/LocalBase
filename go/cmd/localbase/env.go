package main

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/audit"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/conf"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// testHostKey (tests) is the App Context key for a memory.HostProvider that stands in for this
// computer's memory, so what a test checks doesn't depend on what else the machine is running.
// main sets no Context.
type testHostKey struct{}

// hostFor is this computer's memory, or the host a test put in the App's Context.
func hostFor(c *cli.Ctx) memory.HostProvider {
	if h, ok := c.Context().Value(testHostKey{}).(memory.HostProvider); ok {
		return h
	}
	return memory.NewHostProvider()
}

// env is what most commands need: the root, its database, the catalog and
// the configuration. Like TS, opening a root without a config row
// initializes the defaults.
type env struct {
	// DatabaseProblem is why the database could not be read (readEnvKeepingProblems).
	DatabaseProblem error
	Root            *root.Root
	Store           *store.Store
	Catalog         *catalog.Catalog
	Config          *store.Config
	Host            memory.HostProvider
	// audit is where this command's audit records go.
	audit *audit.Log
	// readyWait (tests) is how long a gateway restarted by upgrade gets to answer ready; 0 is
	// upgradeReadyWait.
	readyWait time.Duration
}

func (e *env) Close() {
	if e.Store != nil {
		e.Store.Close()
	}
}

// memoryGB is this machine's memory in whole GiB.
func (e *env) memoryGB() int {
	return int(float64(e.Host.Topology().SystemCapacity)/float64(memory.GiB) + 0.5)
}

func openEnv(c *cli.Ctx) (*env, error) {
	if err := checkOtelEnvironment(); err != nil {
		return nil, err
	}
	r, err := root.Init(root.Resolve(c.Global.Root))
	if err != nil {
		return nil, err
	}
	st, err := store.Open(r.DB())
	if err != nil {
		return nil, err
	}
	e := &env{Root: r, Store: st, Host: hostFor(c), audit: audit.New(r, c.Stderr)}
	if e.Catalog, err = catalog.LoadRoot(r.Dir, nil); err != nil {
		e.Close()
		return nil, err
	}
	cfg, err := st.LoadConfig(context.Background())
	if errors.Is(err, store.ErrNotConfigured) {
		cfg = conf.Default(r.Dir, e.memoryGB(), e.Catalog)
		if err = e.save(cfg); err != nil {
			e.Close()
			return nil, err
		}
	} else if err != nil {
		e.Close()
		return nil, err
	}
	if cfg.Root != r.Dir {
		e.Close()
		return nil, &conf.InvalidError{Root: r.Dir, Issues: []conf.Issue{{Path: "root",
			Message: "root is \"" + cfg.Root + "\" but this database was opened for \"" + r.Dir + "\""}}}
	}
	conf.KeepFormerSelections(r.Dir, cfg, e.Catalog)
	if issues := conf.ValidateModels(cfg, e.Catalog, false); len(issues) > 0 {
		e.Close()
		return nil, &conf.InvalidError{Root: r.Dir, Issues: issues}
	}
	e.Config = cfg
	return e, nil
}

// save validates and writes the config, creating the model directories.
func (e *env) save(cfg *store.Config) error { return e.saveAs(cfg, false) }

// saveAs is save; marking also records a pending restart when the listener
// or memory policy changed (TS persistConfiguration, used by configure), and
// writes settings in the same transaction.
func (e *env) saveAs(cfg *store.Config, marking bool, settings ...store.SettingValue) error {
	if err := conf.Check(cfg, e.Catalog); err != nil {
		return err
	}
	for _, k := range store.Kinds {
		if err := os.MkdirAll(e.Root.ModelsDir(k), 0o700); err != nil {
			return err
		}
	}
	var err error
	switch {
	case marking:
		err = e.Store.SaveConfigMarking(context.Background(), cfg, settings...)
	case len(settings) > 0:
		return errors.New("saveAs: settings are written only with marking")
	default:
		err = e.Store.SaveConfig(context.Background(), cfg)
	}
	if err != nil {
		return err
	}
	e.Config = cfg
	return nil
}

// checkOtelEnvironment rejects invalid OTEL_* variables before a command
// does anything, as TS context creation does.
func checkOtelEnvironment() error {
	if _, err := obs.ResolveOtel("", "", 100, os.Getenv); err != nil {
		return cli.Inputf("%s", err.Error())
	}
	return nil
}
