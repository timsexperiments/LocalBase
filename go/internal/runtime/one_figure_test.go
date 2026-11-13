package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/placement"
	"github.com/timsexperiments/LocalBase/go/internal/placement/placementtest"
)

// A host that is not unified, like the Linux machine where the planner said "uses 5.1 GB"
// and admission then asked for 7.7 GiB: the planner left out the prompt cache, the runtime
// overhead and the slot state.
type cpuHost struct{ avail int64 }

func (h cpuHost) Topology() memory.Topology {
	return memory.Topology{SystemCapacity: 16 * memory.GiB}
}
func (h cpuHost) Snapshot(context.Context) memory.Snapshot {
	return memory.Snapshot{Available: h.avail, Pressure: "normal"}
}

func cpuLLM(t *testing.T, avail int64) *LLM {
	t.Helper()
	mem := memory.NewController(unsampled{cpuHost{avail}}, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	m := newTestLLMWith(t, mem)
	spec := m.d.Catalog.ByID(modelB)
	placementtest.WriteMoEGGUF(t, filepath.Join(m.d.ModelsDir, spec.Primary().Filename), spec.Primary().ExpectedSizeBytes)
	m.SetPlacementSource(&HostPlacement{Store: m.d.Store, DeviceID: "test", Platform: placement.CPUOnly, Memory: mem,
		GPUs: func() []memory.GPUMemory { return nil }})
	return m
}

// The planner and admission give one figure for a CPU load.
func TestCPUPlanReportsTheAdmissionFigure(t *testing.T) {
	m := cpuLLM(t, 60*memory.GiB)
	plan, err := m.PreviewPlacement(modelB, placement.Request{Mode: placement.Auto})
	if err != nil {
		t.Fatal(err)
	}
	p := plan.Placement
	if p == nil || p.GPULayers != 0 {
		t.Fatalf("plan: %+v", p)
	}
	spec := m.d.Catalog.ByID(modelB)
	want := m.admissionDemand(spec, plan).HostBytes
	if p.HostBytes != want {
		t.Errorf("the plan says %d, admission asks for %d", p.HostBytes, want)
	}
	gb := float64(want) / float64(1<<30)
	if !strings.Contains(p.Note, strings.TrimRight(strings.TrimRight(formatTenth(gb), "0"), ".")) {
		t.Errorf("the note disagrees with %.1f GB: %s", gb, p.Note)
	}
}

// A plan that does not fit says so with the same figure, and what was held back.
func TestCPUPlanRefusalNamesTheReserve(t *testing.T) {
	m := cpuLLM(t, 9*memory.GiB)
	_, err := m.PreviewPlacement(modelB, placement.Request{Mode: placement.Auto})
	if err == nil || !strings.Contains(err.Error(), "after keeping 8.0 GB for your computer") {
		t.Errorf("refusal: %v", err)
	}
}

func formatTenth(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }

// Room for the file and its KV cache but not for the rest: the plan must refuse what
// admission would refuse, not say it fits and then be stopped.
func TestCPUPlanCountsWhatAdmissionCounts(t *testing.T) {
	m := cpuLLM(t, 8*memory.GiB+3*memory.GiB+memory.GiB/2)
	if _, err := m.PreviewPlacement(modelB, placement.Request{Mode: placement.Auto}); err == nil {
		t.Error("the plan fits without the prompt cache, runtime and slot state")
	}
}

func TestNextLoadBytesIsAdmissionsFigure(t *testing.T) {
	m := cpuLLM(t, 60*memory.GiB)
	need, ok := m.NextLoadBytes(modelB)
	if !ok {
		t.Fatal("no figure")
	}
	plan, err := m.PreviewPlacement(modelB, placement.Request{Mode: placement.Auto})
	if err != nil {
		t.Fatal(err)
	}
	if want := m.admissionDemand(m.d.Catalog.ByID(modelB), plan).HostBytes; need != want {
		t.Errorf("%d, admission asks for %d", need, want)
	}
}

// A small computer: the refusal is the permanent kind, whatever is free right now.
func TestCPUPlanOnASmallComputerIsTooLarge(t *testing.T) {
	mem := memory.NewController(unsampled{smallHost{}}, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	m := newTestLLMWith(t, mem)
	spec := m.d.Catalog.ByID(modelB)
	placementtest.WriteMoEGGUF(t, filepath.Join(m.d.ModelsDir, spec.Primary().Filename), spec.Primary().ExpectedSizeBytes)
	m.SetPlacementSource(&HostPlacement{Store: m.d.Store, DeviceID: "test", Platform: placement.CPUOnly, Memory: mem,
		GPUs: func() []memory.GPUMemory { return nil }})
	_, err := m.PreviewPlacement(modelB, placement.Request{Mode: placement.Auto})
	var pe *placement.Error
	if !errors.As(err, &pe) || pe.Code != "model_too_large" {
		t.Fatalf("%v", err)
	}
}

type smallHost struct{}

func (smallHost) Topology() memory.Topology { return memory.Topology{SystemCapacity: 10 * memory.GiB} }
func (smallHost) Snapshot(context.Context) memory.Snapshot {
	return memory.Snapshot{Available: 9 * memory.GiB, Pressure: "normal"}
}
