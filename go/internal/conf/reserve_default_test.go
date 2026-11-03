package conf

import (
	"context"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

func defaultReserve(t *testing.T) memory.Reserve {
	t.Helper()
	cat, err := catalog.LoadWithFormer()
	if err != nil {
		t.Fatal(err)
	}
	r := Default("/r", 16, cat).SystemReserve
	return memory.Reserve{Percent: r.Percent, MinimumGB: r.MinimumGB}
}

// Option C (docs/memory-reserve.md): 10%, at least 3 GiB.
func TestDefaultReserveByHostSize(t *testing.T) {
	reserve := defaultReserve(t)
	for _, tc := range []struct {
		hostGiB int64
		want    float64 // GiB
	}{{8, 3}, {16, 3}, {32, 3.2}, {64, 6.4}} {
		got := float64(reserve.Bytes(tc.hostGiB*memory.GiB)) / float64(memory.GiB)
		if got < tc.want-0.001 || got > tc.want+0.001 {
			t.Errorf("%d GiB host: reserve %.3f GiB, want %.1f", tc.hostGiB, got, tc.want)
		}
	}
}

type idleHost struct{ capacity, avail int64 }

func (h idleHost) Topology() memory.Topology { return memory.Topology{SystemCapacity: h.capacity} }
func (h idleHost) Snapshot(context.Context) memory.Snapshot {
	return memory.Snapshot{Available: h.avail, Pressure: "normal"}
}

// The real 16 GB Linux runner, nothing else loaded: the smallest catalog chat model at
// its default context was refused at 15% / 8 GiB. These figures are that machine's own
// (run 41558679605).
func TestDefaultReserveAdmitsTheSmallestChatModelOn16GB(t *testing.T) {
	const capacity, avail, need = int64(16_759_390_208), int64(15_698_255_872), int64(8_266_688_480)
	demand := memory.Demand{HostBytes: need, UnifiedBytes: need, Confidence: "derived"}
	check := func(r memory.Reserve) error {
		c := memory.NewController(idleHost{capacity, avail}, r, false)
		c.SetSampler(nil)
		return c.CheckAdmission(context.Background(), demand, nil)
	}
	if err := check(memory.Reserve{Percent: 15, MinimumGB: 8}); err == nil {
		t.Error("the old default should refuse this, or the test proves nothing")
	}
	if err := check(defaultReserve(t)); err != nil {
		t.Errorf("the default reserve refuses it: %v", err)
	}
}
