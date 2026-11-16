package memory

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

// The sampler is fixed when the Controller is made, from its host, so nothing can change it while
// admission reads it. On main, SetSampler could, and admission raced with it (-race; the PR keeps
// the log). Here two admissions run at once under -race with a host that measures its own processes,
// and that is what they sample.
func TestTheSamplerIsFixedAtConstruction(t *testing.T) {
	_, h := newTestController(60)
	var sampled atomic.Int64
	mh := &measuredHost{fakeHost: h, measure: func(_ context.Context, pid int) (int64, bool) {
		sampled.Store(int64(pid))
		return GiB, true
	}}
	c := NewController(mh, Reserve{Percent: 15, MinimumGB: 8}, false)
	r, err := c.Reserve(context.Background(), "llm", demand(10))
	if err != nil {
		t.Fatal(err)
	}
	r.Materialize(4242)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			for range 200 {
				_ = c.CheckAdmission(context.Background(), demand(1), []string{"llm"})
			}
		})
	}
	wg.Wait()
	if sampled.Load() != 4242 {
		t.Errorf("the host's own sampler wasn't used: sampled pid %d", sampled.Load())
	}

	// A host that doesn't measure its processes is measured with Footprint.
	plain := NewController(h, Reserve{Percent: 15, MinimumGB: 8}, false)
	if reflect.ValueOf(plain.sampler).Pointer() != reflect.ValueOf(Footprint).Pointer() {
		t.Error("a host without a sampler isn't measured with Footprint")
	}
}
