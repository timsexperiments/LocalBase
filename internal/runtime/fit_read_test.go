package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// An installed model's figure needs what was seen of it. A read that fails isn't no peaks.
func TestModelFitReportsAFailedPeakRead(t *testing.T) {
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
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	models := filepath.Join(dir, "models")
	if err := os.MkdirAll(models, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, a := range spec.Artifacts {
		f, err := os.Create(filepath.Join(models, a.Filename))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(a.ExpectedSizeBytes); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	if f, err := ModelFit(context.Background(), st, cat, spec, models, 64); err != nil || f == nil {
		t.Fatalf("%v %v", f, err)
	}
	if _, err := st.DB.Exec(`DROP TABLE model_observed_memory`); err != nil {
		t.Fatal(err)
	}
	_, err = ModelFit(context.Background(), st, cat, spec, models, 64)
	var failed *store.ReadError
	if !errors.As(err, &failed) || failed.Step != "reading the memory peaks seen for "+spec.ModelID {
		t.Errorf("%v", err)
	}
}
