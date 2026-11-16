package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// explainError says a busy database in plain words, whichever command met it, and which step met it.
func explainError(err error) error {
	if !store.IsBusy(err) {
		return err
	}
	var failed *failedRead
	if errors.As(err, &failed) {
		return &cli.ReasonedError{Err: fmt.Errorf("%s: %w", failed.Step, &store.BusyError{Err: err}), Reason: "database_busy"}
	}
	return &cli.ReasonedError{Err: &store.BusyError{Err: err}, Reason: "database_busy"}
}

// failedRead is store.ReadError, under the name this package uses.
type failedRead = store.ReadError

// persistedConfig reads the stored settings for a listing command. A database with none yet leaves
// the defaults to stand in. Any other failure means there are settings it couldn't read, and defaults
// reported as if they were those would be wrong, and exit 0.
func persistedConfig(st *store.Store) (*store.Config, error) {
	cfg, err := st.LoadConfig(context.Background())
	if errors.Is(err, store.ErrNotConfigured) {
		return nil, nil
	}
	if err != nil {
		return nil, &failedRead{Step: "reading the saved settings", Err: err}
	}
	return cfg, nil
}

// unreadableSettings is a database whose settings couldn't be read, in the words the operator approved:
// "Could not read the settings of <root>: <reason>". It unwraps to what failed, so a busy database is
// still told apart.
type unreadableSettings struct {
	Root string
	Err  error
}

func (e *unreadableSettings) Error() string {
	return "Could not read the settings of " + e.Root + ": " + e.Err.Error()
}
func (e *unreadableSettings) Unwrap() error { return e.Err }
