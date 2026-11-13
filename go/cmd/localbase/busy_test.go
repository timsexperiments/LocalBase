package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// recoveringEnv is set for the helper process recoveringElsewhere starts.
const recoveringEnv = "LOCALBASE_TEST_RECOVERING"

// TestRecoveringHelper is that process: it acts like another connection rebuilding the WAL index of
// the database it's given. It holds the -shm's write lock (byte 120) and recovery lock (byte 122),
// and the first copy of the index header no longer matches the second, so a reader in another process
// can neither trust the index nor rebuild it: SQLITE_BUSY_RECOVERY, after the busy timeout. Under WAL a
// writer no longer makes a reader busy, so this is how a busy read happens now; a crashed root's first
// opener beside a second process does it for real. Found by Reliability. It writes a test root's -shm.
func TestRecoveringHelper(t *testing.T) {
	path := os.Getenv(recoveringEnv)
	if path == "" {
		t.Skip("only as the helper process")
	}
	f, err := os.OpenFile(path+"-shm", os.O_RDWR, 0)
	if err != nil {
		fmt.Println("FAILED", err)
		return
	}
	for _, b := range []int64{120, 122} {
		lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: io.SeekStart, Start: b, Len: 1}
		if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lock); err != nil {
			fmt.Println("FAILED", err)
			return
		}
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xFF}, 48), 0); err != nil {
		fmt.Println("FAILED", err)
		return
	}
	fmt.Println("RECOVERING")
	_, _ = io.ReadAll(os.Stdin) // until released
	_ = f.Close()
}

// recoveringElsewhere starts that process on path and returns once it holds the locks. release ends
// it; the next read rebuilds the index from the -wal.
func recoveringElsewhere(t *testing.T, path string) (release func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRecoveringHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), recoveringEnv+"="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	released := false
	release = func() {
		if !released {
			released = true
			_ = stdin.Close()
			_ = cmd.Wait()
		}
	}
	t.Cleanup(release)
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "RECOVERING" {
			return release
		}
		if strings.HasPrefix(line, "FAILED") {
			t.Fatal(line)
		}
	}
	t.Fatal("the helper process ended without taking the locks")
	return release
}

// attachedRoot is a root with a writable store attached, so its -shm exists and a read-only store
// reads it in place, as beside a running gateway.
func attachedRoot(t *testing.T) (dir string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "root")
	r, err := root.Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	attached, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { attached.Close() })
	return dir
}

// The database turning busy after it opened: the settings must not quietly become the defaults.
func TestPersistedConfigOfABusyDatabaseIsAnError(t *testing.T) {
	path := filepath.Join(attachedRoot(t), "local-base.db")
	st, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if cfg, err := persistedConfig(st); err != nil || cfg != nil {
		t.Fatalf("no settings yet: %v %v", cfg, err)
	}
	recoveringElsewhere(t, path)
	if _, err := persistedConfig(st); !store.IsBusy(err) {
		t.Errorf("a busy database: %v", err)
	}
}

// The same, seen by the command: doctor must not go on to report the defaults.
func TestReadEnvStopsWhenTheDatabaseTurnsBusy(t *testing.T) {
	dir := attachedRoot(t)
	arrive := func() { recoveringElsewhere(t, filepath.Join(dir, "local-base.db")) }
	e, err := withoutProblems(readEnvKeepingProblems(&cli.Ctx{Global: cli.Global{Root: dir}}, readOptions{persisted: true, afterOpen: arrive}))
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

// busyRead is a database read in place, beside an attached store; hold makes its next reads busy.
func busyRead(t *testing.T) (*store.Store, func()) {
	t.Helper()
	path := filepath.Join(attachedRoot(t), "local-base.db")
	st, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, func() {
		t.Helper()
		recoveringElsewhere(t, path)
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
	if !strings.HasPrefix(errb.String(), "Database: Could not read the settings of ") || !strings.Contains(errb.String(), "root: opening the database:") {
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
	if !errors.As(err, &failed) || failed.Step != "reading the saved settings" {
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
