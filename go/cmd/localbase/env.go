package main

import (
	"context"
	"errors"
	"os"
	"strconv"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/conf"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// env is what most commands need: the root, its database, the catalog and
// the configuration. Like TS, opening a root without a config row
// initializes the defaults.
type env struct {
	Root    *root.Root
	Store   *store.Store
	Catalog *catalog.Catalog
	Config  *store.Config
	Host    memory.HostProvider
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
	r, err := root.Init(root.Resolve(c.Global.Root))
	if err != nil {
		return nil, err
	}
	st, err := store.Open(r.DB())
	if err != nil {
		return nil, err
	}
	e := &env{Root: r, Store: st, Host: memory.NewHostProvider()}
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
	if issues := conf.ValidateModels(cfg, e.Catalog, false); len(issues) > 0 {
		e.Close()
		return nil, &conf.InvalidError{Root: r.Dir, Issues: issues}
	}
	e.Config = cfg
	return e, nil
}

// save validates and writes the config, creating the model directories.
func (e *env) save(cfg *store.Config) error {
	if err := conf.Check(cfg, e.Catalog); err != nil {
		return err
	}
	for _, k := range store.Kinds {
		if err := os.MkdirAll(e.Root.ModelsDir(k), 0o700); err != nil {
			return err
		}
	}
	if err := e.Store.SaveConfig(context.Background(), cfg); err != nil {
		return err
	}
	e.Config = cfg
	return nil
}

// applyEnvOverrides applies LOCALBASE_HOST, LOCALBASE_PORT, LOCALBASE_STT_HOST,
// LOCALBASE_STT_PORT and LOCALBASE_CTX_SIZE to the in-memory config, as TS
// does for every command. They are never saved.
func applyEnvOverrides(cfg *store.Config) error {
	str := func(name string, dst *string) {
		if v := os.Getenv(name); v != "" {
			*dst = v
		}
	}
	num := func(name string, dst *int, lo, hi int) error {
		v := os.Getenv(name)
		if v == "" {
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi {
			return cli.Inputf("%s: must be an integer from %d to %d", name, lo, hi)
		}
		*dst = n
		return nil
	}
	str("LOCALBASE_HOST", &cfg.Host)
	str("LOCALBASE_STT_HOST", &cfg.SttHost)
	if err := num("LOCALBASE_PORT", &cfg.Port, 1, 65535); err != nil {
		return err
	}
	if err := num("LOCALBASE_STT_PORT", &cfg.SttPort, 1, 65535); err != nil {
		return err
	}
	return num("LOCALBASE_CTX_SIZE", &cfg.CtxSize, 2048, 2_147_483_647)
}
