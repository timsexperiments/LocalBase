package runtime

import (
	"context"
	goruntime "runtime"
	"sync"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

// holdSample makes the next memory sample wait for release, and says when one
// has reached that point.
func holdSample(t *testing.T) (reached, release chan struct{}) {
	t.Helper()
	if goruntime.GOOS != "darwin" {
		t.Skip("memory samples are macOS only")
	}
	reached, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	sampleFootprint = func(context.Context, int) (int64, bool) {
		once.Do(func() { close(reached) })
		<-release
		return memory.GiB, true
	}
	t.Cleanup(func() { sampleFootprint = memory.Footprint })
	return reached, release
}

// closeWaits checks that closeFn does not return while a sample is out, then
// that it does once the sample ends. A sample ends in a store write, so a Close
// that does not wait lets that write land after the database and even the root
// are gone: that is the late local-base.db-journal seen on macOS test teardown.
func closeWaits(t *testing.T, reached, release chan struct{}, closeFn func()) {
	t.Helper()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("no sample started")
	}
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

func TestLLMCloseWaitsForMemorySample(t *testing.T) {
	reached, release := holdSample(t)
	m := newTestLLM(t, &bigHost{avail: 50 * memory.GiB})
	l, err := m.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	closeWaits(t, reached, release, m.Close)
}

func TestServiceCloseWaitsForMemorySample(t *testing.T) {
	reached, release := holdSample(t)
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
	s.d.Gate = func(id string) (*Target, error) {
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
	closeWaits(t, reached, release, s.Close)
	if rows, err := s.d.Store.ObservedPeaks(context.Background(), spec.ModelID, spec.Primary().SHA256); err != nil || len(rows) == 0 {
		t.Errorf("the sample's write: %v %v", rows, err)
	}
}
