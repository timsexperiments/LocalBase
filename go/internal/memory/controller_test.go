package memory

import (
	"context"
	"errors"
	"testing"
)

type fakeHost struct {
	topo  Topology
	avail int64
}

func (f *fakeHost) Topology() Topology { return f.topo }
func (f *fakeHost) Snapshot(context.Context) Snapshot {
	if f.avail < 0 {
		return Snapshot{Available: -1, Pressure: "unknown"}
	}
	return Snapshot{Available: f.avail, Pressure: "normal"}
}

func demand(gb float64) Demand {
	b := int64(gb * float64(GiB))
	return Demand{UnifiedBytes: b, HostBytes: b, AcceleratorBytes: b, Confidence: "measured"}
}

// 64 GiB unified, reserve max(15%, 8 GiB) = 9.6 GiB.
func newTestController(avail float64) (*Controller, *fakeHost) {
	h := &fakeHost{topo: Topology{Unified: true, SystemCapacity: 64 * GiB, GPUWorkingSetBytes: 52 * GiB}, avail: int64(avail * float64(GiB))}
	return NewController(h, Reserve{Percent: 15, MinimumGB: 8}, false), h
}

func reason(err error) string {
	var ae *AdmissionError
	if errors.As(err, &ae) {
		return ae.Reason
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

func TestReserveAdmitsAndRejects(t *testing.T) {
	c, _ := newTestController(30)
	if _, err := c.Reserve(context.Background(), "a", demand(10)); err != nil {
		t.Fatal(err)
	}
	// "a" is pending (not in the sample yet): 30 - 10 - 15 < 9.6.
	if r := reason(func() error { _, err := c.Reserve(context.Background(), "b", demand(15)); return err }()); r != "system-memory" {
		t.Fatalf("want system-memory, got %q", r)
	}
}

func TestPermanentShortfall(t *testing.T) {
	c, _ := newTestController(60)
	_, err := c.Reserve(context.Background(), "x", demand(55))
	var ae *AdmissionError
	if !errors.As(err, &ae) || !ae.Permanent || ae.PoolID != "gpu-working-set" {
		t.Fatalf("got %v", err)
	}
}

// #283: the working-set cap applies to the sum of loaded runtimes.
func TestGPUWorkingSetSum(t *testing.T) {
	c, _ := newTestController(60)
	r, _ := c.Reserve(context.Background(), "a", demand(30))
	r.Materialize(1)
	_, err := c.Reserve(context.Background(), "b", demand(25))
	if reason(err) != "accelerator-memory" {
		t.Fatalf("want accelerator-memory, got %v", err)
	}
}

// #355: a releasing runtime is credited min(projection, footprint).
func TestSwitchPreflightCreditCappedAtFootprint(t *testing.T) {
	c, h := newTestController(30)
	r, err := c.Reserve(context.Background(), "llm", demand(10))
	if err != nil {
		t.Fatal(err)
	}
	r.Materialize(4242)
	h.avail = int64(20 * float64(GiB)) // the resident model now shows in the sample
	next := demand(20.5)               // needs 20.5 + 9.6 reserve = 30.1 GiB

	// Projection credit (10) would admit: 20 + 10 = 30 ... just short; use 20.0 to show the gap.
	c.SetSampler(nil)
	if err := c.CheckAdmission(context.Background(), demand(19.9), []string{"llm"}); err != nil {
		t.Fatalf("projection credit should admit: %v", err)
	}
	// Footprint 7 GiB caps the credit: 20 + 7 - 19.9 < 9.6.
	c.SetSampler(func(_ context.Context, pid int) (int64, bool) {
		if pid != 4242 {
			t.Fatalf("sampled pid %d", pid)
		}
		return 7 * GiB, true
	})
	if r := reason(c.CheckAdmission(context.Background(), demand(19.9), []string{"llm"})); r != "system-memory" {
		t.Fatalf("capped credit should reject, got %q", r)
	}
	// Credit never exceeds the projection even if the footprint is larger.
	c.SetSampler(func(context.Context, int) (int64, bool) { return 40 * GiB, true })
	if r := reason(c.CheckAdmission(context.Background(), next, []string{"llm"})); r != "system-memory" {
		t.Fatalf("credit above projection: got %q", r)
	}
	// Sampler failure falls back to the projection.
	c.SetSampler(func(context.Context, int) (int64, bool) { return 0, false })
	if err := c.CheckAdmission(context.Background(), demand(19.9), []string{"llm"}); err != nil {
		t.Fatalf("fallback should admit: %v", err)
	}
}

func TestUnavailableMeasurementRejects(t *testing.T) {
	c, _ := newTestController(-1)
	if r := reason(func() error { _, err := c.Reserve(context.Background(), "a", demand(1)); return err }()); r != "measurement-unavailable" {
		t.Fatalf("got %q", r)
	}
}

func TestHysteresis(t *testing.T) {
	c, h := newTestController(5) // below the 9.6 reserve: critical
	if a := c.Poll(context.Background()); a != "emergency-stop" {
		t.Fatalf("got %s", a)
	}
	h.avail = 40 * GiB
	for i := 0; i < recoverySamples-1; i++ {
		if a := c.Poll(context.Background()); a != "emergency-stop" {
			t.Fatalf("sample %d: recovered early (%s)", i, a)
		}
		if r := reason(func() error { _, err := c.Reserve(context.Background(), "x", demand(1)); return err }()); r != "memory-pressure" {
			t.Fatalf("admission during recovery: %q", r)
		}
	}
	if a := c.Poll(context.Background()); a != "allow" {
		t.Fatalf("got %s", a)
	}
}

func TestParseVMStatAndFootprint(t *testing.T) {
	vm := []byte("Mach Virtual Memory Statistics: (page size of 16384 bytes)\nPages free:                                   100.\nPages active:      5.\nPages inactive:                              50.\n")
	if b, err := parseVMStat(vm); err != nil || b != 150*16384 {
		t.Fatalf("vm_stat = %d %v", b, err)
	}
	fp := []byte("  Dirty      Clean  Reclaimable    Regions    Category\n2160 KB     544 KB          0 B        409    TOTAL\n")
	if b, ok := parseFootprint(fp); !ok || b != (2160+544)*1024 {
		t.Fatalf("footprint = %d %v", b, ok)
	}
}
