package runtime

import (
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

func cpuPlan(t *testing.T, memGB float64, cpu bool, cfg func(*store.Config)) *LLMPlan {
	t.Helper()
	c := &store.Config{Host: "127.0.0.1", Port: 1, CtxSize: 131072, CtxMode: "auto", Parallel: "auto"}
	if cfg != nil {
		cfg(c)
	}
	m := newTestLLM(t, &bigHost{avail: 50 * memory.GiB})
	spec := m.d.Catalog.ByID(modelB)
	p, err := PlanLLM(PlanInput{Config: c, Spec: spec, ModelID: modelB, ModelsDir: m.d.ModelsDir, MemoryGB: memGB, Port: 1, CPUOnly: cpu})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A CPU-only computer runs a smaller launch by default: a 16 GB machine was given a
// 131072-token context and four slots, which is why the model did not fit and ran slowly.
func TestCPUOnlyDefaultsScaleWithMemory(t *testing.T) {
	for _, tc := range []struct {
		gb         float64
		ctx, slots int
	}{{8, 4096, 1}, {16, 16384, 1}, {32, 32768, 2}} {
		p := cpuPlan(t, tc.gb, true, nil)
		if p.CtxSize > tc.ctx || p.Parallel.Slots > tc.slots {
			t.Errorf("%v GB: context %d with %d slots, want at most %d with %d", tc.gb, p.CtxSize, p.Parallel.Slots, tc.ctx, tc.slots)
		}
	}
	full := cpuPlan(t, 16, false, nil)
	small := cpuPlan(t, 16, true, nil)
	if small.Demand.HostBytes >= full.Demand.HostBytes {
		t.Errorf("the smaller launch needs no less: %d against %d", small.Demand.HostBytes, full.Demand.HostBytes)
	}
}

// A computer with a GPU keeps today's launch, whatever its size.
func TestGPUHostsKeepTheirDefaults(t *testing.T) {
	for _, gb := range []float64{16, 64} {
		a := cpuPlan(t, gb, false, nil)
		m := newTestLLM(t, &bigHost{avail: 50 * memory.GiB})
		if want := RecommendedContext(m.d.Catalog.ByID(modelB), gb); a.CtxSize != min(want, 131072) {
			t.Errorf("%v GB: context %d, want %d", gb, a.CtxSize, want)
		}
	}
}

// What someone set stays set.
func TestCPUOnlyDefaultsYieldToExplicitSettings(t *testing.T) {
	p := cpuPlan(t, 16, true, func(c *store.Config) { c.CtxMode, c.CtxSize, c.Parallel = "explicit", 65536, "2" })
	if p.CtxSize != 65536 || p.Parallel.Slots != 2 {
		t.Errorf("context %d with %d slots", p.CtxSize, p.Parallel.Slots)
	}
}

// One launch for the planner, admission and `models show`.
func TestCPUOnlyScalingReachesThePlan(t *testing.T) {
	m := cpuLLM(t, 15*memory.GiB)
	m.d.MemoryGB = 16
	m.SetConfig(&store.Config{Host: "127.0.0.1", Port: 1, CtxSize: 131072, CtxMode: "auto", Parallel: "auto"})
	plan, err := m.planFor(modelB, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.CtxSize > 16384 || plan.Parallel.Slots != 1 {
		t.Errorf("context %d with %d slots", plan.CtxSize, plan.Parallel.Slots)
	}
	need, ok := m.NextLoadBytes(modelB)
	if !ok || need != m.admissionDemand(m.d.Catalog.ByID(modelB), plan).HostBytes {
		t.Errorf("next load %d %v", need, ok)
	}
}
