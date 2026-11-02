package runtime

import (
	"context"
	"os"
	goruntime "runtime"
	"sync"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
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
	s.d.Gate = func(string) (*Target, error) {
		tgt, err := gate("a")
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
