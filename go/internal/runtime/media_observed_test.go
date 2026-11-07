package runtime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

const sdBackend = "stable-diffusion.cpp-metal"

func imageTarget(t *testing.T, s *Service) (*Target, *catalog.Spec) {
	t.Helper()
	target, err := s.d.Gate(context.Background(), missingImage, false)
	if err != nil {
		t.Fatal(err)
	}
	spec := s.d.Catalog.ByID(missingImage)
	if spec.Primary().SHA256 == "" {
		t.Fatalf("%s needs a hash", missingImage)
	}
	return target, spec
}

func observe(t *testing.T, s *Service, spec *catalog.Spec, shape string, peak int64) {
	t.Helper()
	if err := s.d.Store.RecordObservedPeak(context.Background(), store.ObservedPeak{ModelID: spec.ModelID,
		ArtifactSHA256: spec.Primary().SHA256, LaunchShape: shape, PeakBytes: peak, Method: "footprint"}); err != nil {
		t.Fatal(err)
	}
}

func withMargin(peak int64) int64 { return peak + memory.ObservedMargin(peak) }

// The guard: a model seen only on small requests is not admitted on that. Its next request can be
// larger, and admission doesn't know the request. Over the whole range, the demand is never below
// the estimate nor below the highest peak plus its margin.
func TestAMediaModelSeenSmallIsNotAdmittedBelowItsEstimate(t *testing.T) {
	for _, percent := range []int64{1, 10, 25, 50, 75, 90, 100, 110, 125, 150, 200, 400} {
		s, _, _ := newMissingImageService(t, newStepHost(60*memory.GiB), nil)
		target, spec := imageTarget(t, s)
		estimate := s.demand(target)
		peak := estimate.UnifiedBytes * percent / 100
		observe(t, s, spec, mediaShape(sdBackend), peak)
		got := s.demand(target)
		want := max(estimate.UnifiedBytes, withMargin(peak))
		if got.UnifiedBytes != want || got.HostBytes < estimate.HostBytes || got.AcceleratorBytes < estimate.AcceleratorBytes {
			t.Errorf("peak %d%% of the estimate: demand %+v, want %d", percent, got, want)
		}
		raised := withMargin(peak) > estimate.UnifiedBytes
		if raised != (got.Confidence == "observed") {
			t.Errorf("peak %d%% of the estimate: confidence %q", percent, got.Confidence)
		}
		if !raised && got != estimate {
			t.Errorf("peak %d%% of the estimate changed it: %+v", percent, got)
		}
		if raised && (got.HostBytes < want || got.AcceleratorBytes < want) {
			t.Errorf("peak %d%% of the estimate: %+v", percent, got)
		}
	}
}

// The same, as admission decides it: the boundary doesn't move down, however small the peak.
func TestASmallPeakDoesNotMoveTheAdmissionBoundary(t *testing.T) {
	boundary := missingImageBoundary(t)
	host := newStepHost(boundary - 1)
	s, _, _ := newMissingImageService(t, host, nil)
	_, spec := imageTarget(t, s)
	observe(t, s, spec, mediaShape(sdBackend), memory.GiB/4)
	if _, err := s.Acquire(context.Background(), missingImage); !isInsufficient(err) {
		t.Errorf("just under the estimate's boundary: %v", err)
	}
	host.avail.Store(boundary)
	var ni *NotInstalledError
	if _, err := s.Acquire(context.Background(), missingImage); !errors.As(err, &ni) {
		t.Errorf("at the boundary: %v", err)
	}
}

func isInsufficient(err error) bool {
	var refused *memory.AdmissionError
	return errors.As(err, &refused)
}

// Seen larger than its estimate: refused where it would have loaded and risked pressure.
func TestAMediaModelSeenLargerIsRefusedOnThat(t *testing.T) {
	boundary := missingImageBoundary(t)
	s, _, _ := newMissingImageService(t, newStepHost(boundary), nil)
	target, spec := imageTarget(t, s)
	var ni *NotInstalledError
	if _, err := s.Acquire(context.Background(), missingImage); !errors.As(err, &ni) {
		t.Fatalf("on its estimate, before any peak: %v", err)
	}
	observe(t, s, spec, mediaShape(sdBackend), s.demand(target).UnifiedBytes+4*memory.GiB)
	if _, err := s.Acquire(context.Background(), missingImage); !isInsufficient(err) {
		t.Errorf("seen larger than the memory there is: %v", err)
	}
}

// And it stops idle peers to make the room, where its estimate wouldn't have needed to.
func TestAMediaModelSeenLargerStopsIdlePeers(t *testing.T) {
	boundary := missingImageBoundary(t)
	host := newStepHost(boundary)
	recovery := &IdleRecovery{}
	s, mem, _ := newMissingImageService(t, host, recovery)
	target, spec := imageTarget(t, s)
	peer := newFakePeer(t, mem, host, "llm", 8*memory.GiB, true)
	recovery.Add(peer)
	var ni *NotInstalledError
	if _, err := s.Acquire(context.Background(), missingImage); !errors.As(err, &ni) {
		t.Fatalf("on its estimate: %v", err)
	}
	if peer.stopped {
		t.Fatal("its estimate fitted, so nothing should have stopped")
	}
	estimate := s.demand(target).UnifiedBytes
	observe(t, s, spec, mediaShape(sdBackend), estimate+memory.GiB)
	if got := s.demand(target).UnifiedBytes; got <= estimate {
		t.Fatalf("demand %d, estimate %d", got, estimate)
	}
	if _, err := s.Acquire(context.Background(), missingImage); !errors.As(err, &ni) {
		t.Errorf("after stopping the peer: %v", err)
	}
	if !peer.stopped {
		t.Error("the idle peer was not stopped to make room")
	}
}

// The highest seen, never the last.
func TestTheHighestPeakCountsNotTheLast(t *testing.T) {
	s, _, _ := newMissingImageService(t, newStepHost(60*memory.GiB), nil)
	target, spec := imageTarget(t, s)
	high := s.demand(target).UnifiedBytes * 2
	for _, peak := range []int64{high / 4, high, high / 2} {
		observe(t, s, spec, mediaShape(sdBackend), peak)
	}
	if got := s.demand(target).UnifiedBytes; got != withMargin(high) {
		t.Errorf("demand %d, want %d", got, withMargin(high))
	}
	// Even if the store didn't keep only the highest, and the order of the rows doesn't matter.
	shape := mediaShape(sdBackend)
	for _, peaks := range [][]store.ObservedPeak{
		{{LaunchShape: shape, PeakBytes: 9, Method: "footprint"}, {LaunchShape: "default", PeakBytes: 3 * memory.GiB, Method: "footprint"}},
		{{LaunchShape: "default", PeakBytes: 3 * memory.GiB, Method: "footprint"}, {LaunchShape: shape, PeakBytes: 9, Method: "footprint"}},
	} {
		if got := RaiseToObserved(memory.Demand{}, peaks, shape).UnifiedBytes; got != withMargin(3*memory.GiB) {
			t.Errorf("%+v: %d", peaks, got)
		}
	}
}

// What doesn't count: another backend, another way of measuring. Peaks from before backends were
// told apart do count, as they can only raise it.
func TestWhichPeaksCount(t *testing.T) {
	const peak = 20 * memory.GiB
	estimate := memory.Demand{UnifiedBytes: memory.GiB, HostBytes: memory.GiB, AcceleratorBytes: memory.GiB, Confidence: "estimated"}
	shape := mediaShape(sdBackend)
	for _, c := range []struct {
		name   string
		peak   store.ObservedPeak
		raises bool
	}{
		{"this backend", store.ObservedPeak{LaunchShape: shape, PeakBytes: peak, Method: "footprint"}, true},
		{"from before backends were told apart", store.ObservedPeak{LaunchShape: "default", PeakBytes: peak, Method: "footprint"}, true},
		{"another backend", store.ObservedPeak{LaunchShape: mediaShape("other"), PeakBytes: peak, Method: "footprint"}, false},
		{"not a footprint", store.ObservedPeak{LaunchShape: shape, PeakBytes: peak, Method: "vmhwm"}, false},
		{"nothing measured", store.ObservedPeak{LaunchShape: shape, PeakBytes: 0, Method: "footprint"}, false},
	} {
		got := RaiseToObserved(estimate, []store.ObservedPeak{c.peak}, shape)
		if raised := got.UnifiedBytes > estimate.UnifiedBytes; raised != c.raises {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
}

// Another file's peak says nothing, and neither does an imported model's.
func TestPeaksAreForThisFile(t *testing.T) {
	s, _, _ := newMissingImageService(t, newStepHost(60*memory.GiB), nil)
	target, spec := imageTarget(t, s)
	estimate := s.demand(target)
	if err := s.d.Store.RecordObservedPeak(context.Background(), store.ObservedPeak{ModelID: spec.ModelID,
		ArtifactSHA256: strings.Repeat("0", 64), LaunchShape: mediaShape(sdBackend), PeakBytes: 50 * memory.GiB, Method: "footprint"}); err != nil {
		t.Fatal(err)
	}
	if got := s.demand(target); got != estimate {
		t.Errorf("another file's peak: %+v", got)
	}
	observe(t, s, spec, mediaShape(sdBackend), 50*memory.GiB)
	imported := *target
	imported.Imported = true
	if got := s.demand(&imported); got.Confidence == "observed" {
		t.Errorf("an imported model: %+v", got)
	}
}

// A store that can't be read leaves the estimate, and says so.
func TestAFailedPeakReadKeepsTheEstimateAndSaysSo(t *testing.T) {
	s, _, _ := newMissingImageService(t, newStepHost(60*memory.GiB), nil)
	target, spec := imageTarget(t, s)
	estimate := s.demand(target)
	observe(t, s, spec, mediaShape(sdBackend), 50*memory.GiB)
	var logs strings.Builder
	s.d.Log = slog.New(slog.NewTextHandler(&logs, nil))
	if err := s.d.Store.Close(); err != nil {
		t.Fatal(err)
	}
	if got := s.demand(target); got != estimate {
		t.Errorf("demand %+v, want the estimate %+v", got, estimate)
	}
	if !strings.Contains(logs.String(), "memory.observed-read-failed") {
		t.Errorf("logs: %s", logs.String())
	}
	s.d.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
}

// The model's page says what admission does.
func TestTheFitFigureOfAMediaModelIsWhatAdmissionUses(t *testing.T) {
	s, _, _ := newMissingImageService(t, newStepHost(60*memory.GiB), nil)
	_, spec := imageTarget(t, s)
	before := FitFor(spec, 64, nil, nil, nil)
	small, large := int64(1<<20), before.ReferenceBytes*2
	peak := func(shape string, b int64) store.ObservedPeak {
		return store.ObservedPeak{LaunchShape: shape, PeakBytes: b, Method: "footprint"}
	}
	if got := FitFor(spec, 64, nil, []store.ObservedPeak{peak("default", small)}, nil); got.DemandBytes != before.ReferenceBytes {
		t.Errorf("seen small: figure %d, reference %d", got.DemandBytes, before.ReferenceBytes)
	}
	got := FitFor(spec, 64, nil, []store.ObservedPeak{peak(mediaShape(sdBackend), small), peak("default", large)}, nil)
	if got.DemandBytes != large || got.Source != "observed" {
		t.Errorf("seen large: %+v", got)
	}
}

// A peak is kept under the backend that ran it, which is what admission looks for.
func TestAMediaPeakIsRecordedForItsBackend(t *testing.T) {
	if goruntime.GOOS != "darwin" {
		t.Skip("footprints are sampled on macOS")
	}
	before := sampleFootprint
	sampleFootprint = func(context.Context, int) (int64, bool) { return 7 * memory.GiB, true }
	t.Cleanup(func() { sampleFootprint = before })
	s, spec := startService(t)
	target, err := s.d.Gate(context.Background(), spec.ModelID, false)
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		peaks, err := s.d.Store.ObservedPeaks(context.Background(), spec.ModelID, spec.Primary().SHA256)
		if err != nil {
			t.Fatal(err)
		}
		if len(peaks) > 0 {
			if peaks[0].LaunchShape != mediaShape(target.Backend.ID) || peaks[0].PeakBytes != 7*memory.GiB {
				t.Errorf("recorded %+v", peaks[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no peak was recorded")
		}
	}
	if got := s.demand(target).UnifiedBytes; got < withMargin(7*memory.GiB) {
		t.Errorf("the demand doesn't use what was recorded: %d", got)
	}
}
