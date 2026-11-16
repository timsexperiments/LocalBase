package runtime

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// The fit uses the peaks the store has seen for the model's file; with no store it is the catalog's;
// and a read that fails still gives a fit, with the error beside it.
func TestObservedFit(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	var s *catalog.Spec
	for _, sp := range cat.All() {
		if sp.Kind == "llm" && sp.Primary().SHA256 != "" {
			s = sp
			break
		}
	}
	if s == nil {
		t.Fatal("no LLM with a file hash in the catalog")
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	plain := FitFor(s, 64, nil, nil, cat.PlannableFeedPeaks(s))
	if f, err := ObservedFit(ctx, nil, cat, s, 64); err != nil || !reflect.DeepEqual(f, plain) {
		t.Errorf("no store: %+v, %v", f, err)
	}
	if f, err := ObservedFit(ctx, st, cat, s, 64); err != nil || !reflect.DeepEqual(f, plain) {
		t.Errorf("no peaks yet: %+v, %v", f, err)
	}
	peak := store.ObservedPeak{ModelID: s.ModelID, ArtifactSHA256: s.Primary().SHA256, LaunchShape: plain.LaunchShape,
		PeakBytes: 3 << 30, Method: "footprint", ObservedAt: "2026-01-01T00:00:00.000Z"}
	if err := st.RecordObservedPeak(ctx, peak); err != nil {
		t.Fatal(err)
	}
	seen := FitFor(s, 64, nil, []store.ObservedPeak{peak}, cat.PlannableFeedPeaks(s))
	f, err := ObservedFit(ctx, st, cat, s, 64)
	if err != nil || f.Source != "observed" || f.ObservedPeakBytes == nil || *f.ObservedPeakBytes != 3<<30 || f.MinVramGB != seen.MinVramGB {
		t.Errorf("with a peak: %+v, %v", f, err)
	}
	st.Close()
	if f, err := ObservedFit(ctx, st, cat, s, 64); err == nil || f == nil || f.Source == "observed" {
		t.Errorf("a failed read: %+v, %v", f, err)
	}
}
