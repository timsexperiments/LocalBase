package runtime

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

// TestCatalogMemoryMatchesTypeScript compares every built-in model's
// estimate, measured demand and fit figure with tools/golden/catalog-memory.ts.
func TestCatalogMemoryMatchesTypeScript(t *testing.T) {
	b, err := os.ReadFile("../memory/testdata/catalog-memory.json")
	if err != nil {
		t.Fatal(err)
	}
	type demand struct{ UnifiedBytes, HostBytes, AcceleratorBytes int64 }
	var cases []struct {
		ModelID     string
		Estimate    demand
		Measured    *demand
		MinVramGb   int
		Source      string
		LaunchShape string
	}
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	cat, _ := catalog.Load("")
	for _, c := range cases {
		s := cat.ByID(c.ModelID)
		e := memory.EstimateCatalog(s, nil)
		if got := (demand{e.UnifiedBytes, e.HostBytes, e.AcceleratorBytes}); got != c.Estimate {
			t.Errorf("%s estimate: go %+v ts %+v", c.ModelID, got, c.Estimate)
		}
		m, ok := memory.MeasuredCatalog(s, nil)
		switch {
		case ok != (c.Measured != nil):
			t.Errorf("%s measured: go %v ts %v", c.ModelID, ok, c.Measured != nil)
		case ok && (demand{m.UnifiedBytes, m.HostBytes, m.AcceleratorBytes}) != *c.Measured:
			t.Errorf("%s measured: go %+v ts %+v", c.ModelID, m, *c.Measured)
		}
		f := FitFor(s, 64, nil, nil, nil)
		if f.MinVramGB != c.MinVramGb || f.Source != c.Source || f.LaunchShape != c.LaunchShape {
			t.Errorf("%s fit: go %d/%s/%s ts %d/%s/%s", c.ModelID, f.MinVramGB, f.Source, f.LaunchShape, c.MinVramGb, c.Source, c.LaunchShape)
		}
	}
}
