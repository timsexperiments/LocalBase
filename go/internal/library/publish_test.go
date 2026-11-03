package library

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/acquire"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/testutil"
)

// modelBytes is a small file an import accepts.
func modelBytes(t *testing.T) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "m.gguf")
	if err := testutil.GGUF(p, map[string]any{"general.architecture": "qwen3", "general.file_type": 15, "qwen3.context_length": 4096}, 4096); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// gatedSource sends a file, holding back the last of it until the gate opens, so a
// download can be seen running and then let finish.
func gatedSource(t *testing.T, gate <-chan struct{}) *httptest.Server {
	t.Helper()
	body := modelBytes(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "bytes=0-0" || r.Method == http.MethodHead {
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			if r.Method == http.MethodHead {
				return
			}
			w.Write(body[:1])
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Write(body[:len(body)/2])
		w.(http.Flusher).Flush()
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		w.Write(body[len(body)/2:])
	}))
	t.Cleanup(srv.Close)
	return srv
}

func listed(t *testing.T, l *Library, id string) bool {
	t.Helper()
	rows, err := l.Store.ImportedModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}

// importedFiles counts the files in place for a model.
func importedFiles(t *testing.T, l *Library, id string) int {
	t.Helper()
	entries, err := os.ReadDir(acquire.ImportedDir(l.Root.Dir, id))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// Two downloads and a configuration change don't get in each other's way, and each ends
// up whole. Publishing waits for the root while something else holds it.
func TestTwoDownloadsAndAConfigureInterleave(t *testing.T) {
	l := newTestLibrary(t)
	gate := make(chan struct{})
	src := gatedSource(t, gate)
	a, err := l.StartImport(planFor(t, l, src.URL, "first"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := l.StartImport(planFor(t, l, src.URL, "second"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, l, a, downloading)
	waitJob(t, l, b, downloading)

	// Mid-download, a change takes the root at once.
	start := time.Now()
	err = root.WithOperation(l.Root.Dir, "update runtime configuration", func() error {
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("waited %v for the root while two imports downloaded", took)
		}
		// With the root held, let both finish. They must wait, not fail.
		close(gate)
		time.Sleep(300 * time.Millisecond)
		for _, id := range []string{a, b} {
			if j := l.Job(id); j.State != "running" {
				t.Errorf("import finished while the root was held: %+v", j.State)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for id, job := range map[string]string{"first": a, "second": b} {
		j := waitJob(t, l, job, ended)
		if j.State != "done" || j.Model == nil || j.Model.ID != id {
			t.Fatalf("%s: %+v (%s)", id, j.State, j.Error)
		}
		if !listed(t, l, id) || importedFiles(t, l, id) != 1 {
			t.Errorf("%s: listed %v, %d files", id, listed(t, l, id), importedFiles(t, l, id))
		}
	}
	if n := stagedFiles(t, l); n != 0 {
		t.Errorf("%d files left in staging", n)
	}
}

// A finished download isn't thrown away because the root is busy: hours of download may
// be behind it. A person can still stop it while it waits.
func TestPublishWaitsForABusyRoot(t *testing.T) {
	busy := fmt.Errorf("held: %w", root.ErrOperationBusy)
	realLease := withLease
	t.Cleanup(func() { withLease = realLease; leaseRetryEvery = time.Second })
	leaseRetryEvery = 5 * time.Millisecond

	t.Run("it waits", func(t *testing.T) {
		l := newTestLibrary(t)
		var tries atomic.Int32
		withLease = func(dir, op string, fn func() error) error {
			if tries.Add(1) <= 3 {
				return busy
			}
			return realLease(dir, op, fn)
		}
		gate := make(chan struct{})
		close(gate)
		id, err := l.StartImport(planFor(t, l, gatedSource(t, gate).URL, "patient"), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if j := waitJob(t, l, id, ended); j.State != "done" {
			t.Fatalf("%s: %s", j.State, j.Error)
		}
		if tries.Load() < 4 {
			t.Errorf("%d tries", tries.Load())
		}
	})
	t.Run("a cancel still works", func(t *testing.T) {
		l := newTestLibrary(t)
		var held atomic.Bool
		withLease = func(dir, op string, fn func() error) error {
			held.Store(true)
			return busy
		}
		gate := make(chan struct{})
		close(gate)
		id, err := l.StartImport(planFor(t, l, gatedSource(t, gate).URL, "stopped"), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for !held.Load() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		l.CancelImport(id)
		j := waitJob(t, l, id, ended)
		if j.State != "failed" || j.ErrorCode != acquire.CodeCancelled {
			t.Errorf("%s %s", j.State, j.ErrorCode)
		}
		if listed(t, l, "stopped") || stagedFiles(t, l) != 0 {
			t.Errorf("a cancelled import left something: listed %v, %d staged", listed(t, l, "stopped"), stagedFiles(t, l))
		}
	})
	t.Run("any other failure is not retried", func(t *testing.T) {
		l := newTestLibrary(t)
		var tries atomic.Int32
		withLease = func(dir, op string, fn func() error) error {
			tries.Add(1)
			return errors.New("the disk is on fire")
		}
		gate := make(chan struct{})
		close(gate)
		id, err := l.StartImport(planFor(t, l, gatedSource(t, gate).URL, "broken"), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if j := waitJob(t, l, id, ended); j.State != "failed" || !strings.Contains(j.Error, "on fire") {
			t.Errorf("%s: %s", j.State, j.Error)
		}
		if tries.Load() != 1 {
			t.Errorf("%d tries", tries.Load())
		}
	})
}

// TestPublishKillHelper is the process the kill test kills. It only runs when asked.
func TestPublishKillHelper(t *testing.T) {
	stage := os.Getenv("LB_PUBLISH_KILL_AT")
	if stage == "" {
		t.Skip("run by TestKilledWhilePublishing")
	}
	publishHook = func(at string) {
		if at == stage {
			syscall.Kill(os.Getpid(), syscall.SIGKILL)
			time.Sleep(time.Minute)
		}
	}
	l := openLibrary(t, os.Getenv("LB_PUBLISH_ROOT"))
	id, err := l.StartImport(planFor(t, l, os.Getenv("LB_PUBLISH_SOURCE"), "killed"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, l, id, ended)
	t.Fatalf("still alive after the import ended: %+v", l.Job(id))
}

func openLibrary(t *testing.T, dir string) *Library {
	t.Helper()
	r, err := root.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cat, _ := catalog.LoadWithFormer()
	l, err := Open(r, st, cat, nil)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// A process killed at any step of publishing leaves a model that is listed and whole, or
// one that is not listed at all and can be imported again. Never half of one.
func TestKilledWhilePublishing(t *testing.T) {
	gate := make(chan struct{})
	close(gate)
	src := gatedSource(t, gate)
	for _, tc := range []struct {
		stage          string
		listedAfter    bool
		filesAfter     int
		retryCompletes bool
	}{
		{"before-publish", false, 0, true},
		{"after-publish", false, 1, true}, // moved, not recorded: the case that needs care
		{"after-record", true, 1, false},  // done: a second import of it is refused
	} {
		t.Run(tc.stage, func(t *testing.T) {
			l := newTestLibrary(t)
			cmd := exec.Command(os.Args[0], "-test.run=^TestPublishKillHelper$", "-test.timeout=60s")
			cmd.Env = append(os.Environ(), "LB_PUBLISH_KILL_AT="+tc.stage, "LB_PUBLISH_ROOT="+l.Root.Dir, "LB_PUBLISH_SOURCE="+src.URL, "XDG_RUNTIME_DIR="+filepath.Dir(l.Root.Dir))
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || !strings.Contains(exit.Error(), "killed") {
				t.Fatalf("the helper wasn't killed: %v\n%s", err, out)
			}
			if got := listed(t, l, "killed"); got != tc.listedAfter {
				t.Errorf("listed: %v, want %v", got, tc.listedAfter)
			}
			if got := importedFiles(t, l, "killed"); got != tc.filesAfter {
				t.Errorf("%d files in place, want %d", got, tc.filesAfter)
			}
			// Whatever the kill left, what is listed is whole.
			if listed(t, l, "killed") && importedFiles(t, l, "killed") == 0 {
				t.Error("listed with no files")
			}
			// The killed process's lease doesn't stand in the way of what comes next.
			id, err := l.StartImport(planFor(t, l, src.URL, "killed"), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			j := waitJob(t, l, id, ended)
			if tc.retryCompletes {
				if j.State != "done" {
					t.Fatalf("the retry: %s: %s", j.State, j.Error)
				}
				if !listed(t, l, "killed") || importedFiles(t, l, "killed") != 1 {
					t.Errorf("after the retry: listed %v, %d files", listed(t, l, "killed"), importedFiles(t, l, "killed"))
				}
			} else if j.State != "failed" || !strings.Contains(j.Error, "already imported") {
				t.Errorf("importing a finished model again: %s: %s", j.State, j.Error)
			}
			if listed(t, l, "killed") {
				if _, err := os.Stat(acquire.ImportedDir(l.Root.Dir, "killed")); err != nil {
					t.Errorf("a recorded model lost its files: %v", err)
				}
			}
		})
	}
}

// The move and the record happen under one hold of the root, and a cancel that comes
// after the move doesn't stop the record: that would leave the files with nothing listing them.
func TestPublishIsOneStepUnderTheRoot(t *testing.T) {
	l := newTestLibrary(t)
	realLease, realHook := withLease, publishHook
	t.Cleanup(func() { withLease, publishHook = realLease, realHook })
	var inside atomic.Bool
	withLease = func(dir, op string, fn func() error) error {
		return realLease(dir, op, func() error {
			inside.Store(true)
			defer inside.Store(false)
			return fn()
		})
	}
	var job atomic.Value
	var steps atomic.Int32
	publishHook = func(step string) {
		if !inside.Load() {
			t.Errorf("%s: outside the root's lease", step)
		}
		steps.Add(1)
		if step == "after-publish" {
			if id, ok := job.Load().(string); ok {
				l.CancelImport(id)
			}
		}
	}
	gate := make(chan struct{})
	id, err := l.StartImport(planFor(t, l, gatedSource(t, gate).URL, "both"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	job.Store(id)
	close(gate)
	j := waitJob(t, l, id, ended)
	if steps.Load() != 3 {
		t.Errorf("%d steps", steps.Load())
	}
	if !listed(t, l, "both") || importedFiles(t, l, "both") != 1 {
		t.Errorf("a cancel after the move: %s (%s), listed %v, %d files", j.State, j.Error, listed(t, l, "both"), importedFiles(t, l, "both"))
	}
}

// Files can appear after the import started, from another process's crash: publishing
// clears them too, under the lease that makes checking them safe.
func TestPublishClearsWhatAppearedDuringTheDownload(t *testing.T) {
	l := newTestLibrary(t)
	gate := make(chan struct{})
	id, err := l.StartImport(planFor(t, l, gatedSource(t, gate).URL, "late"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, l, id, downloading)
	dest := acquire.ImportedDir(l.Root.Dir, "late")
	if err := os.MkdirAll(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "left-behind"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	close(gate)
	if j := waitJob(t, l, id, ended); j.State != "done" {
		t.Fatalf("%s: %s", j.State, j.Error)
	}
	if _, err := os.Stat(filepath.Join(dest, "left-behind")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("what was left behind is still there: %v", err)
	}
}

// Clearing what a crash left only ever touches files nothing records.
func TestReclaimLeavesRecordedModelsAlone(t *testing.T) {
	l := newTestLibrary(t)
	ctx := context.Background()
	dest := acquire.ImportedDir(l.Root.Dir, "kept")
	if err := os.MkdirAll(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "m.gguf"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.Store.SaveImportedModel(ctx, "kept", []byte(`{"id":"kept"}`)); err != nil {
		t.Fatal(err)
	}
	if err := l.reclaim(ctx, "kept"); err != nil {
		t.Fatal(err)
	}
	if importedFiles(t, l, "kept") != 1 {
		t.Error("removed a recorded model's files")
	}
	// If the record can't be read, nothing is removed: not knowing isn't the same as no.
	orphan := acquire.ImportedDir(l.Root.Dir, "orphan")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	broken, err := store.Open(filepath.Join(t.TempDir(), "other.db"))
	if err != nil {
		t.Fatal(err)
	}
	broken.Close()
	good := l.Store
	l.Store = broken
	err = l.reclaim(ctx, "orphan")
	l.Store = good
	if err == nil {
		t.Error("no error from an unreadable record")
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Errorf("removed files with no way to check them: %v", err)
	}
	// Otherwise they go.
	if err := l.reclaim(ctx, "orphan"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an orphan stayed: %v", err)
	}
	// No directory is nothing to do.
	if err := l.reclaim(ctx, "absent"); err != nil {
		t.Error(err)
	}
}

// The limits come from this root's settings, and the defaults hold without them.
func TestImportLimitsFromSettings(t *testing.T) {
	l := newTestLibrary(t)
	ctx := context.Background()
	plan := &acquire.Plan{}
	if got := l.ImportLimits(ctx, plan); got != acquire.ImportDeadline || plan.MaxUnknownBytes != 0 {
		t.Errorf("defaults: %v %d", got, plan.MaxUnknownBytes)
	}
	if err := l.Store.SetSetting(ctx, store.ImportDeadlineKey, "12"); err != nil {
		t.Fatal(err)
	}
	if err := l.Store.SetSetting(ctx, store.ImportMaxUnknownKey, "512"); err != nil {
		t.Fatal(err)
	}
	if got := l.ImportLimits(ctx, plan); got != 12*time.Hour || plan.MaxUnknownBytes != 512<<30 {
		t.Errorf("set: %v %d", got, plan.MaxUnknownBytes)
	}
	// Something unreadable leaves the defaults in place, never no limit.
	for _, bad := range []string{"0", "-1", "lots", ""} {
		l.Store.SetSetting(ctx, store.ImportDeadlineKey, bad)
		l.Store.SetSetting(ctx, store.ImportMaxUnknownKey, bad)
		plan := &acquire.Plan{}
		if got := l.ImportLimits(ctx, plan); got != acquire.ImportDeadline || plan.MaxUnknownBytes != 0 {
			t.Errorf("%q: %v %d", bad, got, plan.MaxUnknownBytes)
		}
	}
}

// A running import keeps to what the root sets.
func TestStartedImportUsesTheRootsLimits(t *testing.T) {
	l := newTestLibrary(t)
	ctx := context.Background()
	old := acquire.ImportDeadline
	acquire.ImportDeadline = 50 * time.Millisecond
	t.Cleanup(func() { acquire.ImportDeadline = old })
	if err := l.Store.SetSetting(ctx, store.ImportDeadlineKey, "1"); err != nil {
		t.Fatal(err)
	}
	if err := l.Store.SetSetting(ctx, store.ImportMaxUnknownKey, "7"); err != nil {
		t.Fatal(err)
	}
	id, err := l.StartImport(planFor(t, l, slowSource(t).URL, "limited"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Past the default deadline, it is still running: an hour was set.
	time.Sleep(300 * time.Millisecond)
	j := l.Job(id)
	if j.State != "running" {
		t.Errorf("the setting wasn't used for the deadline: %s %s", j.State, j.Error)
	}
	if j.Plan.MaxUnknownBytes != 7<<30 {
		t.Errorf("the plan's limit: %d", j.Plan.MaxUnknownBytes)
	}
	l.CancelImport(id)
	waitJob(t, l, id, ended)
}

// Clearing what a crash left takes the root first: without it, this could remove files
// another process has moved into place and is about to record.
func TestPrepareTakesTheRootBeforeClearing(t *testing.T) {
	l := newTestLibrary(t)
	ctx := context.Background()
	dest := acquire.ImportedDir(l.Root.Dir, "left")
	if err := os.MkdirAll(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	realLease := withLease
	t.Cleanup(func() { withLease = realLease })
	var inside atomic.Int32
	withLease = func(dir, op string, fn func() error) error {
		inside.Add(1)
		if _, err := os.Stat(dest); err != nil {
			t.Errorf("cleared before taking the root: %v", err)
		}
		return realLease(dir, op, fn)
	}
	if err := l.PrepareImport(ctx, &acquire.Plan{ID: "left"}); err != nil {
		t.Fatal(err)
	}
	if inside.Load() != 1 {
		t.Errorf("took the root %d times", inside.Load())
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("not cleared: %v", err)
	}
	// With nothing to clear, it doesn't take the root at all.
	if err := l.PrepareImport(ctx, &acquire.Plan{ID: "nothing"}); err != nil {
		t.Fatal(err)
	}
	if inside.Load() != 1 {
		t.Errorf("took the root for nothing: %d", inside.Load())
	}
}
