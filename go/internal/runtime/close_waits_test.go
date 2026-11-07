package runtime

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// holdSample makes the next memory sample wait, and says when one has reached
// that point. A sample that honors cancel ends with its context, as the real one
// does (it kills the tool it runs); one that ignores it ends only on release.
func holdSample(t *testing.T, honorCancel bool) (reached, release chan struct{}) {
	t.Helper()
	if goruntime.GOOS != "darwin" {
		t.Skip("memory samples are macOS only")
	}
	reached, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	sampleFootprint = func(ctx context.Context, _ int) (int64, bool) {
		once.Do(func() { close(reached) })
		if honorCancel {
			select {
			case <-ctx.Done():
				return 0, false
			case <-release:
			}
		} else {
			<-release
		}
		return memory.GiB, true
	}
	t.Cleanup(func() { sampleFootprint = memory.Footprint })
	return reached, release
}

// closeWaits: Close does not return while a sample is out, even one that ignores
// its cancel, because a sample can end in a store write. Without this, that write
// lands after the database and the root are gone: the late local-base.db-journal
// seen on macOS test teardown.
func closeWaits(t *testing.T, reached, release chan struct{}, closeFn func()) {
	t.Helper()
	waitReached(t, reached)
	closed := make(chan struct{})
	go func() { closeFn(); close(closed) }()
	select {
	case <-closed:
		t.Error("Close returned with a memory sample still out")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close never returned")
	}
}

func waitReached(t *testing.T, reached chan struct{}) {
	t.Helper()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("no sample started")
	}
}

// closeIsPrompt: launchd and the restart path wait on Close, so it signals a
// running sample and does not sit out the sample's 4 s limit.
func closeIsPrompt(t *testing.T, reached chan struct{}, closeFn func()) {
	t.Helper()
	waitReached(t, reached)
	began := time.Now()
	closeFn()
	if d := time.Since(began); d > 500*time.Millisecond {
		t.Errorf("Close took %v with a memory sample out", d)
	}
}

func startLLM(t *testing.T) *LLM {
	t.Helper()
	m := newTestLLM(t, &bigHost{avail: 50 * memory.GiB})
	l, err := m.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	return m
}

func startService(t *testing.T) (*Service, *catalog.Spec) {
	t.Helper()
	s, _ := newTestService(t)
	var spec *catalog.Spec
	for _, c := range s.d.Catalog.All() {
		if c.Kind == "stt" && c.Primary().SHA256 != "" {
			spec = c
			break
		}
	}
	if spec == nil {
		t.Fatal("no catalog speech model with a checksum")
	}
	// The sample only runs for a catalog model: serve the fake under its id.
	gate := s.d.Gate
	s.d.Gate = func(ctx context.Context, _ string, install bool) (*Target, error) {
		tgt, err := gate(ctx, "a", install)
		if err == nil {
			tgt.ModelID = spec.ModelID
		}
		return tgt, err
	}
	l, err := s.Acquire(context.Background(), spec.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	return s, spec
}

func TestLLMCloseWaitsForMemorySample(t *testing.T) {
	reached, release := holdSample(t, false)
	closeWaits(t, reached, release, startLLM(t).Close)
}

func TestServiceCloseWaitsForMemorySample(t *testing.T) {
	reached, release := holdSample(t, false)
	s, spec := startService(t)
	closeWaits(t, reached, release, s.Close)
	// The sample came back after the stop: nothing is written.
	if rows, err := s.d.Store.ObservedPeaks(context.Background(), spec.ModelID, spec.Primary().SHA256); err != nil || len(rows) != 0 {
		t.Errorf("a stopped sample wrote: %v %v", rows, err)
	}
}

func TestLLMClosePromptDuringMemorySample(t *testing.T) {
	reached, _ := holdSample(t, true)
	closeIsPrompt(t, reached, startLLM(t).Close)
}

func TestServiceClosePromptDuringMemorySample(t *testing.T) {
	reached, _ := holdSample(t, true)
	s, _ := startService(t)
	closeIsPrompt(t, reached, s.Close)
}

// The real sampler stops when asked: it kills the tool it runs.
func TestFootprintHonorsCancel(t *testing.T) {
	if goruntime.GOOS != "darwin" {
		t.Skip("memory samples are macOS only")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	began := time.Now()
	if _, ok := memory.Footprint(ctx, os.Getpid()); ok {
		t.Error("a cancelled sample returned a reading")
	}
	if d := time.Since(began); d > 500*time.Millisecond {
		t.Errorf("a cancelled sample took %v", d)
	}
}

// A peak that can't be written isn't lost silently, and a later sample writes it, though it's no
// higher than the one that failed.
func TestAPeakThatCannotBeWrittenComesRoundAgain(t *testing.T) {
	if goruntime.GOOS != "darwin" {
		t.Skip("memory samples are macOS only")
	}
	var bytes atomic.Int64
	bytes.Store(memory.GiB)
	sampleFootprint = func(context.Context, int) (int64, bool) { return bytes.Load(), true }
	t.Cleanup(func() { sampleFootprint = memory.Footprint })
	previous := samplePeriod
	samplePeriod = 20 * time.Millisecond
	t.Cleanup(func() { samplePeriod = previous })
	s, spec := startService(t)
	st := s.d.Store
	ctx := context.Background()
	highest := func() int64 {
		rows, err := st.ObservedPeaks(ctx, spec.ModelID, spec.Primary().SHA256)
		if err != nil {
			t.Fatal(err)
		}
		var top int64
		for _, r := range rows {
			top = max(top, r.PeakBytes)
		}
		return top
	}
	until := func(what string, done func() bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); !done(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
		}
	}
	until("the first peak", func() bool { return highest() == memory.GiB })
	var mu sync.Mutex
	var logged strings.Builder
	st.Log = slog.New(slog.NewJSONHandler(writerFunc(func(b []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return logged.Write(b)
	}), nil))
	if _, err := st.DB.ExecContext(ctx, `CREATE TRIGGER planted_i BEFORE INSERT ON model_observed_memory BEGIN SELECT RAISE(ABORT, 'planted'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `CREATE TRIGGER planted_u BEFORE UPDATE ON model_observed_memory BEGIN SELECT RAISE(ABORT, 'planted'); END`); err != nil {
		t.Fatal(err)
	}
	bytes.Store(2 * memory.GiB)
	until("no word of the failed write", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(logged.String(), "store.observed-peak-write-failed")
	})
	for _, trigger := range []string{"planted_i", "planted_u"} {
		if _, err := st.DB.ExecContext(ctx, `DROP TRIGGER `+trigger); err != nil {
			t.Fatal(err)
		}
	}
	until("the peak never got written", func() bool { return highest() == 2*memory.GiB })
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

// What a runtime reports about itself that can't be stored is said at a level anyone sees: the caller
// logs a probe that failed only for debugging.
func TestCapabilitiesThatCannotBeStoredAreNotSilent(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var logged strings.Builder
	st.Log = slog.New(slog.NewJSONHandler(&logged, nil))
	if _, err := st.DB.Exec(`CREATE TRIGGER planted BEFORE INSERT ON model_runtime_capabilities BEGIN SELECT RAISE(ABORT, 'planted'); END`); err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.LoadWithFormer()
	if err != nil {
		t.Fatal(err)
	}
	var spec *catalog.Spec
	for _, c := range cat.All() {
		if c.Kind == "llm" && c.Primary().SHA256 != "" && !c.IsEmbedding() && !c.IsDecision() {
			spec = c
			break
		}
	}
	if spec == nil {
		t.Fatal("no catalog model with a checksum")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"build_info":"b","data":[]}`))
	}))
	defer srv.Close()
	if _, err := ProbeLlamaServer(context.Background(), st, srv.URL, spec); err == nil {
		t.Error("a failed write should still be an error")
	}
	if !strings.Contains(logged.String(), "store.runtime-capabilities-write-failed") {
		t.Errorf("%q", logged.String())
	}
}
