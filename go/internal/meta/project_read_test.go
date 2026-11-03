package meta

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

func TestProjectReportsAFailedPeakRead(t *testing.T) {
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
	in := Input{Store: st, RootDir: dir, Catalog: cat, MemoryGB: 64, CLI: true, Config: &store.Config{Active: map[string]string{}, Selected: map[string][]string{}}}
	if _, problems := Project(context.Background(), in, spec); len(problems) != 0 {
		t.Fatalf("before: %v", problems)
	}
	if _, err := st.DB.Exec(`DROP TABLE model_observed_memory`); err != nil {
		t.Fatal(err)
	}
	m, problems := Project(context.Background(), in, spec)
	if len(problems) == 0 || problems[0].Kind != store.ReadPeaks {
		t.Errorf("%v", problems)
	}
	mem := m.Catalog.Memory
	if mem.Fit.Verdict != "unknown" || mem.DemandBytes != 0 || mem.Source != "unavailable" || mem.StorageEstimateGb != spec.StorageGB {
		t.Errorf("%+v", mem)
	}
}
