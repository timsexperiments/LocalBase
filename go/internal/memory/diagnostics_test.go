package memory

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"reflect"
	"testing"
)

type goldenDemand struct {
	UnifiedGiB     float64 `json:"unifiedGiB"`
	AcceleratorGiB float64 `json:"acceleratorGiB"`
	Confidence     string  `json:"confidence"`
}

func (g goldenDemand) demand() Demand {
	b := func(v float64) int64 { return int64(math.Round(v * float64(GiB))) }
	return Demand{UnifiedBytes: b(g.UnifiedGiB), HostBytes: b(g.UnifiedGiB), AcceleratorBytes: b(g.AcceleratorGiB), Confidence: g.Confidence}
}

// attributeMap is an error's attributes as JSON would carry them, with the
// timestamps reduced to their type as in the golden.
func attributeMap(t *testing.T, err error) map[string]any {
	t.Helper()
	var ae *AdmissionError
	if !errors.As(err, &ae) {
		t.Fatalf("not an admission error: %v", err)
	}
	attrs := ae.Attributes()
	m := map[string]any{}
	for i := 0; i < len(attrs); i += 2 {
		m[attrs[i].(string)] = attrs[i+1]
	}
	for _, k := range []string{"sample_captured_at_ms", "sample_age_ms"} {
		if _, ok := m[k].(int64); ok {
			m[k] = "number"
		}
	}
	raw, _ := json.Marshal(m)
	out := map[string]any{}
	json.Unmarshal(raw, &out)
	return out
}

// TestAdmissionDiagnosticsGolden replays the TS operations
// (tools/golden/admission-diagnostics.ts): every refusal carries the same
// runtime.memory-admission-rejected attributes as TS.
func TestAdmissionDiagnosticsGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/admission-diagnostics.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Scenarios map[string]struct {
			Ops []struct {
				Op          string       `json:"op"`
				GiB         *float64     `json:"gib"`
				ID          string       `json:"id"`
				Demand      goldenDemand `json:"demand"`
				Materialize bool         `json:"materialize"`
				Releasing   []string     `json:"releasing"`
			} `json:"ops"`
			Results []map[string]any `json:"results"`
		} `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for name, sc := range golden.Scenarios {
		t.Run(name, func(t *testing.T) {
			h := &fakeHost{topo: Topology{Unified: true, SystemCapacity: 64 * GiB, GPUWorkingSetBytes: 48 * GiB}}
			c := NewController(h, Reserve{Percent: 15, MinimumGB: 8}, false)
			c.SetSampler(nil)
			reservations := map[string]*Reservation{}
			var got []map[string]any
			result := func(err error) {
				if err == nil {
					got = append(got, nil)
					return
				}
				got = append(got, attributeMap(t, err))
			}
			for _, op := range sc.Ops {
				switch op.Op {
				case "available":
					if op.GiB == nil {
						h.avail = -1
					} else {
						h.avail = int64(math.Round(*op.GiB * float64(GiB)))
					}
				case "poll":
					c.Poll(ctx)
				case "reserve":
					r, err := c.Reserve(ctx, op.ID, op.Demand.demand())
					if err != nil {
						t.Fatal(err)
					}
					if op.Materialize {
						r.Materialize(4242)
					}
					reservations[op.ID] = r
				case "release":
					reservations[op.ID].Release()
				case "check":
					result(c.CheckAdmission(ctx, op.Demand.demand(), op.Releasing))
				case "start":
					r, err := c.Reserve(ctx, op.ID, op.Demand.demand())
					if err == nil {
						r.Release()
					}
					result(err)
				}
			}
			if !reflect.DeepEqual(got, sc.Results) {
				g, _ := json.MarshalIndent(got, "", " ")
				w, _ := json.MarshalIndent(sc.Results, "", " ")
				t.Fatalf("attributes\n%s\nwant\n%s", g, w)
			}
		})
	}
}
