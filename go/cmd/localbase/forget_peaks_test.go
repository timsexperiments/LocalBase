package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// The way out of a peak that over-refuses: it goes, for every backend and file of that model, and
// no other model's.
func TestForgetPeaksClearsOneModel(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	dir := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", dir, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	r, err := root.Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	record := func(model, sha, shape string) {
		t.Helper()
		if err := st.RecordObservedPeak(ctx, store.ObservedPeak{ModelID: model, ArtifactSHA256: sha, LaunchShape: shape, PeakBytes: 1 << 30, Method: "footprint"}); err != nil {
			t.Fatal(err)
		}
	}
	record("a-model", "sha1", "default")
	record("a-model", "sha1", "backend=x")
	record("a-model", "sha2", "backend=y")
	record("other", "sha1", "default")
	code, out := runApp(t, "--root", dir, "models", "forget-peaks", "a-model")
	if code != 0 || !strings.Contains(out, "Forgot 3 memory peaks for a-model") {
		t.Errorf("%d: %s", code, out)
	}
	for _, sha := range []string{"sha1", "sha2"} {
		if peaks, err := st.ObservedPeaks(ctx, "a-model", sha); err != nil || len(peaks) != 0 {
			t.Errorf("%s: %+v %v", sha, peaks, err)
		}
	}
	if peaks, err := st.ObservedPeaks(ctx, "other", "sha1"); err != nil || len(peaks) != 1 {
		t.Errorf("another model's: %+v %v", peaks, err)
	}
	if code, out := runApp(t, "--root", dir, "models", "forget-peaks", "a-model"); code != 0 || !strings.Contains(out, "No memory peaks") {
		t.Errorf("again: %d %s", code, out)
	}
	if code, out := runApp(t, "--root", dir, "--json", "models", "forget-peaks", "other"); code != 0 || !strings.Contains(out, `"forgotten":1`) {
		t.Errorf("json: %d %s", code, out)
	}
}
