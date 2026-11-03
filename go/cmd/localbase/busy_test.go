package main

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// A writer arriving after the database opened: the settings must not quietly become the defaults.
func TestPersistedConfigOfABusyDatabaseIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-base.db")
	created, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	created.Close()
	st, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if cfg, err := persistedConfig(st); err != nil || cfg != nil {
		t.Fatalf("no settings yet: %v %v", cfg, err)
	}
	writer, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	writer.SetMaxOpenConns(1)
	if _, err := writer.Exec(`BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	if _, err := persistedConfig(st); !store.IsBusy(err) {
		t.Errorf("a busy database: %v", err)
	}
}

// The same writer, seen by the command: doctor must not go on to report the defaults.
func TestReadEnvStopsWhenTheDatabaseTurnsBusy(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "root")
	r, err := root.Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	created.Close()
	writer, err := sql.Open("sqlite", "file:"+r.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	writer.SetMaxOpenConns(1)
	afterReadOpen = func() {
		if _, err := writer.Exec(`BEGIN EXCLUSIVE`); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { afterReadOpen = func() {} }()
	e, err := readEnv(&cli.Ctx{Global: cli.Global{Root: dir}}, true)
	if !store.IsBusy(err) {
		t.Errorf("readEnv: %v, %v", e, err)
	}
}

func TestExplainErrorOnlyChangesABusyDatabase(t *testing.T) {
	other := errors.New("something else")
	if got := explainError(other, &cli.Ctx{}); got != other {
		t.Errorf("another failure became: %v", got)
	}
}
