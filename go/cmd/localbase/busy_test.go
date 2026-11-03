package main

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
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
	if got := explainError(other); got != other {
		t.Errorf("another failure became: %v", got)
	}
}

// Stopping the service disables it, which is far too much to suggest for a lock that will pass.
func TestABusyDatabaseNamesNoCommandToRun(t *testing.T) {
	said := (&store.BusyError{}).Error()
	if strings.Contains(said, "local-base") {
		t.Errorf("says: %s", said)
	}
}

// busyRead is a database that a writer holds, for reads the command makes after it opened.
func busyRead(t *testing.T) (*store.Store, func()) {
	t.Helper()
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
	t.Cleanup(func() { st.Close() })
	writer, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Close() })
	writer.SetMaxOpenConns(1)
	return st, func() {
		t.Helper()
		if _, err := writer.Exec(`BEGIN EXCLUSIVE`); err != nil {
			t.Fatal(err)
		}
	}
}

// A busy read of a model's memory peaks used to give figures with none, and of its voices a page with
// none, as if nothing had been seen. Each now says which step it was.
func TestABusyReadNamesItsStep(t *testing.T) {
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	var withSHA, speech *catalog.Spec
	for _, s := range cat.All() {
		if withSHA == nil && s.Primary().SHA256 != "" {
			withSHA = s
		}
		if speech == nil && s.Kind == "tts" {
			speech = s
		}
	}
	for name, read := range map[string]struct {
		step string
		call func(e *env) error
	}{
		"peaks":    {"reading the memory peaks seen for", func(e *env) error { _, err := cliFit(e, withSHA, 64); return err }},
		"voices":   {"reading the custom voices", func(e *env) error { _, err := cliVoices(e, speech); return err }},
		"settings": {"reading the saved settings", func(e *env) error { _, err := persistedConfig(e.Store); return err }},
	} {
		st, hold := busyRead(t)
		e := &env{Store: st, Catalog: cat}
		if err := read.call(e); err != nil {
			t.Errorf("%s, before the writer: %v", name, err)
		}
		hold()
		err := read.call(e)
		if !store.IsBusy(err) {
			t.Errorf("%s: %v", name, err)
			continue
		}
		var reasoned *cli.ReasonedError
		explained := explainError(err)
		if !errors.As(explained, &reasoned) || reasoned.Reason != "database_busy" {
			t.Errorf("%s: %#v", name, explained)
		}
		if !strings.Contains(explained.Error(), read.step) || !strings.Contains(explained.Error(), "The database is busy") {
			t.Errorf("%s: %q", name, explained.Error())
		}
	}
}

// doctor on a database it can't read: that first and plain, everything else it can still say, and a
// failing exit. Exit 0 with default settings was the bug.
func TestDoctorReportsAnUnreadableDatabaseAndCarriesOn(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "root")
	r, err := root.Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.DB(), []byte("this is not a database, and is long enough to be read as one"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	a := app()
	a.Stdout, a.Stderr = &out, &errb
	if code := a.Run([]string{"--root", dir, "doctor"}); code != 1 {
		t.Errorf("exit %d", code)
	}
	if !strings.HasPrefix(errb.String(), "Database: opening the database:") {
		t.Errorf("stderr: %s", errb.String())
	}
	for _, want := range []string{"LocalBase:", "OS:", "RAM:"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q: %s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "Parallel Slots") {
		t.Errorf("reports the defaults as settings: %s", out.String())
	}
	out.Reset()
	errb.Reset()
	a = app()
	a.Stdout, a.Stderr = &out, &errb
	if code := a.Run([]string{"--root", dir, "--json", "doctor"}); code != 1 {
		t.Errorf("--json exit %d", code)
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	problem, _ := env.Data["databaseProblem"].(map[string]any)
	if problem["reason"] != "database_unreadable" || env.Data["hardware"] == nil || env.Data["configuration"] != nil {
		t.Errorf("%v", env.Data)
	}
	// Other commands have nothing to say without it.
	if code, said := runApp(t, "--root", dir, "models", "catalog"); code == 0 || !strings.Contains(said, "opening the database") {
		t.Errorf("models catalog: %d %s", code, said)
	}
}

// diagnostics still makes the bundle, and says in it and on the console what it couldn't read.
func TestDiagnosticsReportsWhatItCouldNotRead(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "root")
	r, err := root.Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.DB(), []byte("this is not a database, and is long enough to be read as one"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "report.zip")
	code, said := runApp(t, "--root", dir, "diagnostics", "--output", bundle)
	if code != 0 {
		t.Fatalf("%d: %s", code, said)
	}
	if !strings.Contains(said, "Could not read: opening the database:") {
		t.Errorf("console: %s", said)
	}
	zr, err := zip.OpenReader(bundle)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	found := false
	for _, f := range zr.File {
		if f.Name != "manifest.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		found = strings.Contains(string(b), `"readProblems"`) && strings.Contains(string(b), "opening the database")
		if strings.Contains(string(b), dir) && strings.Contains(string(b), "not a database") {
			t.Log("the root path appears elsewhere in the manifest, as it did")
		}
	}
	if !found {
		t.Error("the manifest doesn't list the read that failed")
	}
}

// And a peak read that fails partway, which used to be silent.
func TestDiagnosticModelsListsAFailedPeakRead(t *testing.T) {
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	var spec *catalog.Spec
	for _, s := range cat.All() {
		if s.Primary().SHA256 != "" {
			spec = s
			break
		}
	}
	st, hold := busyRead(t)
	hold()
	problems := &readProblems{dir: t.TempDir()}
	cfg := &store.Config{Active: map[string]string{spec.Kind: spec.ModelID}, Selected: map[string][]string{spec.Kind: {spec.ModelID}}}
	diagnosticModels(context.Background(), problems.dir, st, cat, cfg, 64, problems)
	if len(problems.text) == 0 || !strings.Contains(strings.Join(problems.text, "\n"), "reading the memory peaks seen for "+spec.ModelID+": the database is busy") {
		t.Errorf("%v", problems.text)
	}
}

// A database that opens but can't give its settings is the same: the settings exist.
func TestSettingsThatCannotBeReadAreAnError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "root")
	r, err := root.Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := created.DB.Exec(`DROP TABLE config`); err != nil {
		t.Fatal(err)
	}
	created.Close()
	st, err := store.OpenReadOnly(r.DB())
	if err != nil {
		t.Skipf("the database doesn't open after that, which the other test covers: %v", err)
	}
	defer st.Close()
	_, err = persistedConfig(st)
	var failed *failedRead
	if !errors.As(err, &failed) || failed.step != "reading the saved settings" {
		t.Errorf("%v", err)
	}
}

// No database yet, or none configured, is still the defaults.
func TestNoSettingsYetIsStillTheDefaults(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", dir, "doctor"); code != 0 {
		t.Errorf("no root at all: %d %s", code, out)
	}
	r, err := root.Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	created.Close()
	if code, out := runApp(t, "--root", dir, "doctor"); code != 0 {
		t.Errorf("a database with no settings: %d %s", code, out)
	}
}

// The same habit that has bitten three times: a store read whose error is thrown away reads as nothing
// there. New ones fail here; the list would say why each exception is one, and is empty.
func TestNoStoreReadThrowsAwayItsError(t *testing.T) {
	exceptions := map[string]string{}
	swallowed := regexp.MustCompile(`, _ :?= (e\.Store|st|store)\.[A-Z]\w*\(`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		if m := swallowed.Find(b); m != nil {
			if _, ok := exceptions[f]; !ok {
				t.Errorf("%s throws away a store error: %s", f, m)
			}
		} else if _, ok := exceptions[f]; ok {
			t.Errorf("%s no longer needs its exception", f)
		}
	}
	if checked < 10 {
		t.Fatalf("read only %d files", checked)
	}
}
