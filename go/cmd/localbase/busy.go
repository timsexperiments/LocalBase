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
		return &cli.ReasonedError{Err: fmt.Errorf("%s: %w", failed.step, &store.BusyError{Err: err}), Reason: "database_busy"}
	}
	return &cli.ReasonedError{Err: &store.BusyError{Err: err}, Reason: "database_busy"}
}

// failedRead is a read a command needed and couldn't do. It says which, so that nothing goes on as
// if there were nothing there.
type failedRead struct {
	step string
	err  error
}

func (e *failedRead) Error() string { return e.step + ": " + e.err.Error() }
func (e *failedRead) Unwrap() error { return e.err }

// afterReadOpen is for tests: a writer that arrives just after the database opened.
var afterReadOpen = func() {}

// persistedConfig reads the stored settings for a listing command. A database with none yet leaves
// the defaults to stand in. Any other failure means there are settings it couldn't read, and defaults
// reported as if they were those would be wrong, and exit 0.
func persistedConfig(st *store.Store) (*store.Config, error) {
	cfg, err := st.LoadConfig(context.Background())
	if errors.Is(err, store.ErrNotConfigured) {
		return nil, nil
	}
	if err != nil {
		return nil, &failedRead{"reading the saved settings", err}
	}
	return cfg, nil
}
