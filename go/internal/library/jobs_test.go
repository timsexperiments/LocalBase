package library

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/acquire"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

func newTestLibrary(t *testing.T) *Library {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", dir)
	r, err := root.Init(filepath.Join(dir, "root"))
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
	t.Cleanup(func() { l.StopImports(5 * time.Second) })
	return l
}

// slowSource keeps sending until the client goes away.
func slowSource(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 16<<10)
		// Bounded, so a test that never stops it fails instead of filling the disk. It then
		// holds the download open, so it never finishes before a test's cancel lands.
		for sent := 0; sent < 64<<20; sent += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			if r.Header.Get("Range") == "bytes=0-0" {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func planFor(t *testing.T, l *Library, source, id string) *acquire.Plan {
	t.Helper()
	p, err := acquire.Resolve(context.Background(), l.Root.Dir, acquire.Request{Source: source + "/" + id + ".gguf", AllowHTTP: true, AllowPrivate: true, Confirmed: true, ID: id, Task: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func waitJob(t *testing.T, l *Library, id string, cond func(*Job) bool) *Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		j := l.Job(id)
		if j != nil && cond(j) {
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %+v", j)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func downloading(j *Job) bool { return j.State == "running" && j.Progress.Done > 0 }
func ended(j *Job) bool       { return j.State != "running" }

func stagedFiles(t *testing.T, l *Library) int {
	t.Helper()
	n := 0
	filepath.WalkDir(filepath.Join(l.Root.Dir, "models", ".staging"), func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}

// A person can stop an import, and what it downloaded goes with it.
func TestCancelStopsAnImportAndRemovesItsFiles(t *testing.T) {
	l := newTestLibrary(t)
	id, err := l.StartImport(planFor(t, l, slowSource(t).URL, "c1"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, l, id, downloading)
	if !l.CancelImport(id) {
		t.Fatal("not found")
	}
	j := waitJob(t, l, id, ended)
	if j.State != "cancelled" || j.ErrorCode != acquire.CodeCancelled || j.Error != "" || j.Detail != "The import was cancelled, and what it downloaded was removed." {
		t.Errorf("%+v", j)
	}
	if n := stagedFiles(t, l); n != 0 {
		t.Errorf("%d files left in staging", n)
	}
	if l.CancelImport("imp_none") {
		t.Error("cancelled a job that does not exist")
	}
	// Cancelling a finished job changes nothing.
	if !l.CancelImport(id) || l.Job(id).ErrorCode != acquire.CodeCancelled {
		t.Error("a finished job")
	}
}

// A cancel that could not remove the download says so, instead of saying it did.
func TestCancelSaysWhenItCouldNotRemoveTheFiles(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the mode this test relies on")
	}
	l := newTestLibrary(t)
	var logged bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logged, nil))
	id, err := l.StartImport(planFor(t, l, slowSource(t).URL, "c3"), "", log)
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, l, id, downloading)
	staging := filepath.Join(l.Root.Dir, "models", ".staging")
	if err := os.Chmod(staging, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(staging, 0o700) })
	if !l.CancelImport(id) {
		t.Fatal("not found")
	}
	j := waitJob(t, l, id, ended)
	if j.State != "cancelled" || j.ErrorCode != acquire.CodeCancelled || j.Error != "" {
		t.Errorf("%+v", j)
	}
	if !strings.Contains(j.Detail, "could not be removed") || strings.Contains(j.Detail, "was removed") {
		t.Errorf("detail: %q", j.Detail)
	}
	if !strings.Contains(logged.String(), `"event":"model.import-cleanup-failed"`) || !strings.Contains(logged.String(), `"level":"WARN"`) {
		t.Errorf("not logged: %s", logged.String())
	}
	if !strings.Contains(logged.String(), `"event":"model.import-cancelled"`) {
		t.Errorf("the cancel itself was not logged: %s", logged.String())
	}
}

// A person cancelling a job that has no log still gets the same answer.
func TestCancelWithNoLogWhenItCouldNotRemoveTheFiles(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the mode this test relies on")
	}
	l := newTestLibrary(t)
	id, err := l.StartImport(planFor(t, l, slowSource(t).URL, "c4"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, l, id, downloading)
	staging := filepath.Join(l.Root.Dir, "models", ".staging")
	if err := os.Chmod(staging, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(staging, 0o700) })
	l.CancelImport(id)
	if j := waitJob(t, l, id, ended); !strings.Contains(j.Detail, "could not be removed") {
		t.Errorf("%+v", j)
	}
}

// A download does not hold the root: a configuration change can happen meanwhile.
func TestADownloadDoesNotHoldTheRoot(t *testing.T) {
	l := newTestLibrary(t)
	id, err := l.StartImport(planFor(t, l, slowSource(t).URL, "c2"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, l, id, downloading)
	start := time.Now()
	if err := root.WithOperation(l.Root.Dir, "update runtime configuration", func() error { return nil }); err != nil {
		t.Fatalf("after %v: %v", time.Since(start), err)
	}
	l.CancelImport(id)
	waitJob(t, l, id, ended)
}

func TestImportsAreCapped(t *testing.T) {
	l := newTestLibrary(t)
	src := slowSource(t).URL
	var ids []string
	for i := range maxActiveImports {
		id, err := l.StartImport(planFor(t, l, src, "cap"+string(rune('a'+i))), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if _, err := l.StartImport(planFor(t, l, src, "capz"), "", nil); !errors.Is(err, ErrImportsBusy) {
		t.Errorf("past the cap: %v", err)
	}
	if _, err := l.StartImport(planFor(t, l, src, "capa"), "", nil); !errors.Is(err, ErrImportRunning) {
		t.Errorf("the same model twice: %v", err)
	}
	for _, id := range ids {
		l.CancelImport(id)
		waitJob(t, l, id, ended)
	}
	if id, err := l.StartImport(planFor(t, l, src, "capz"), "", nil); err != nil {
		t.Errorf("after the others ended: %v", err)
	} else {
		l.CancelImport(id)
		waitJob(t, l, id, ended)
	}
}

// An import that keeps moving but never finishes ends at the deadline, and can resume.
func TestImportsHaveADeadline(t *testing.T) {
	old := acquire.ImportDeadline
	acquire.ImportDeadline = 300 * time.Millisecond
	defer func() { acquire.ImportDeadline = old }()
	l := newTestLibrary(t)
	id, err := l.StartImport(planFor(t, l, slowSource(t).URL, "d1"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	j := waitJob(t, l, id, ended)
	if j.ErrorCode != acquire.CodeTimedOut {
		t.Errorf("%+v", j)
	}
	if n := stagedFiles(t, l); n != 1 {
		t.Errorf("%d staged files, want the partial kept", n)
	}
}

// A shutdown ends imports and keeps what they downloaded, to resume after a restart.
func TestStopImportsKeepsThePartial(t *testing.T) {
	l := newTestLibrary(t)
	id, err := l.StartImport(planFor(t, l, slowSource(t).URL, "s1"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, l, id, downloading)
	l.StopImports(5 * time.Second)
	j := l.Job(id)
	if j.State != "failed" || j.ErrorCode == acquire.CodeCancelled {
		t.Errorf("%+v", j)
	}
	if n := stagedFiles(t, l); n != 1 {
		t.Errorf("%d staged files, want the partial kept", n)
	}
}

// Finished jobs do not pile up.
func TestFinishedJobsAreForgotten(t *testing.T) {
	var j jobs
	j.byID = map[string]*Job{}
	base := time.Now()
	for i := range keptFinishedImports + 10 {
		id := string(rune('a' + i))
		j.byID[id] = &Job{ID: id, State: "done", Started: base.Add(time.Duration(i) * time.Second)}
		j.trim()
	}
	if len(j.byID) != keptFinishedImports {
		t.Errorf("%d kept", len(j.byID))
	}
	if j.byID["a"] != nil {
		t.Error("the oldest was kept")
	}
}
