package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/placement"
	"github.com/timsexperiments/LocalBase/go/internal/placement/placementtest"
)

// stubPlacement is a discrete GPU with settable free memory.
type stubPlacement struct {
	mu   sync.Mutex
	req  placement.Request
	free int64
}

func (s *stubPlacement) Request(string) placement.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.req
}

func (s *stubPlacement) Device(*Target) placement.Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	return placement.Device{Platform: placement.Discrete, FreeVRAM: s.free, HostAvailable: 64 << 30, CPUMoE: true, GPULayers: true}
}

// newPlacedLLM is the test LLM with modelA's file replaced by the MoE GGUF,
// on a discrete GPU too small for the whole model.
func newPlacedLLM(t *testing.T) (*LLM, *stubPlacement) {
	t.Helper()
	m := newTestLLM(t, &bigHost{avail: 50 * memory.GiB})
	spec := m.d.Catalog.ByID(modelA)
	placementtest.WriteMoEGGUF(t, filepath.Join(m.d.ModelsDir, spec.Primary().Filename), spec.Primary().ExpectedSizeBytes)
	full := spec.Primary().ExpectedSizeBytes + placement.WorkspaceBytes + placement.ReserveBytes
	stub := &stubPlacement{req: placement.Request{Mode: placement.Auto}, free: full}
	m.d.Placement = stub
	return m, stub
}

func TestPlacementAutoOffloadsExperts(t *testing.T) {
	m, stub := newPlacedLLM(t)
	l, err := m.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	_, plan, _ := m.Running()
	p := plan.Placement
	if p == nil || p.CPUMoELayers != 1 || p.GPULayers != p.TotalLayers || !slices.Equal(p.Args, []string{"--n-cpu-moe", "1"}) || plan.PlacedAt.IsZero() {
		t.Fatalf("placement %+v (placed %v)", p, plan.PlacedAt)
	}
	if args := plan.Args(false); !slices.Contains(args, "--n-cpu-moe") {
		t.Errorf("launch args %v", args)
	}
	// The plan belongs to the load: more free VRAM does not change it.
	stub.mu.Lock()
	stub.free = 1 << 40
	stub.mu.Unlock()
	l, err = m.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	if _, again, _ := m.Running(); again.Placement != p {
		t.Errorf("the placement was re-resolved without a reload: %+v", again.Placement)
	}
}

func TestPlacementMismatchFailsClosed(t *testing.T) {
	for env, val := range map[string]string{"FAKE_PLACEMENT_BUG": "cuda0", "FAKE_COMPUTE_MIB": "900"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv(env, val)
			m, _ := newPlacedLLM(t)
			_, err := m.Acquire(context.Background(), modelA)
			var pe *placement.Error
			if !errors.As(err, &pe) || pe.Code != "placement_mismatch" {
				t.Fatalf("acquire: %v", err)
			}
			if id, _, _ := m.Running(); id != "" {
				t.Errorf("%s is still running after a mismatch", id)
			}
		})
	}
}

func TestPlacementGPUOnlyNeverFallsBack(t *testing.T) {
	m, stub := newPlacedLLM(t)
	stub.req = placement.Request{Mode: placement.GPU}
	_, err := m.Acquire(context.Background(), modelA)
	var pe *placement.Error
	if !errors.As(err, &pe) || pe.Code != "placement_does_not_fit" {
		t.Fatalf("gpu-only on a GPU too small: %v", err)
	}
	if id, _, _ := m.Running(); id != "" {
		t.Errorf("%s started", id)
	}
}

// A discrete placement reserves its GPU bytes on gpu:0 before starting; too
// little free VRAM now is an ordinary, retryable admission refusal.
func TestPlacementGPUPool(t *testing.T) {
	m, stub := newPlacedLLM(t)
	free := stub.free
	m.d.GPUPool = &memory.GPUPool{Reserve: placement.ReserveBytes, Sample: func() []memory.GPUMemory {
		return []memory.GPUMemory{{Index: 0, FreeBytes: free, TotalBytes: 24 << 30}}
	}}
	l, err := m.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatalf("enough free VRAM: %v", err)
	}
	l.Release()
	m.Close()

	m, _ = newPlacedLLM(t)
	m.d.GPUPool = &memory.GPUPool{Reserve: placement.ReserveBytes, Sample: func() []memory.GPUMemory {
		return []memory.GPUMemory{{Index: 0, FreeBytes: 1 << 30, TotalBytes: 24 << 30}} // freed by something else since planning
	}}
	_, err = m.Acquire(context.Background(), modelA)
	var ae *memory.AdmissionError
	if !errors.As(err, &ae) || ae.PoolID != "gpu:0" || ae.Permanent {
		t.Fatalf("gpu:0 refusal: %v", err)
	}
	if id, _, _ := m.Running(); id != "" {
		t.Errorf("%s started", id)
	}
}
