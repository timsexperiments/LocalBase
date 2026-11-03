package main

import (
	"context"
	"fmt"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// explainError says a busy database in plain words, whichever command met it.
func explainError(err error, c *cli.Ctx) error {
	if !store.IsBusy(err) {
		return err
	}
	busy := &store.BusyError{Err: err}
	return &cli.ReasonedError{
		Err:    fmt.Errorf("%s If it keeps happening, stop the gateway first: local-base --root '%s' stop", busy.Error(), root.Resolve(c.Global.Root)),
		Reason: "database_busy",
	}
}

// afterReadOpen is for tests: a writer that arrives just after the database opened.
var afterReadOpen = func() {}

// persistedConfig reads the stored settings for a listing command. A database that can't give any
// leaves the defaults to stand in. A writer can arrive after the database opened, and a busy one
// has settings, so it is an error.
func persistedConfig(st *store.Store) (*store.Config, error) {
	cfg, err := st.LoadConfig(context.Background())
	if store.IsBusy(err) {
		return nil, err
	}
	if err != nil {
		return nil, nil
	}
	return cfg, nil
}
