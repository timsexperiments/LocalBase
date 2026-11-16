package memory

import (
	"errors"
	"testing"
)

// The gpu:0 pool (DESIGN §20.4): admit or refuse against free VRAM now,
// minus the reserve and the loads not yet resident.
func TestGPUPool(t *testing.T) {
	free := int64(10 << 30)
	p := &GPUPool{Reserve: 512 << 20, Sample: func() []GPUMemory { return []GPUMemory{{Index: 0, FreeBytes: free, TotalBytes: 12 << 30}} }}
	a, err := p.Admit("llm", 6<<30)
	if err != nil {
		t.Fatal(err)
	}
	// A second load while the first is still starting counts its pending bytes.
	_, err = p.Admit("image", 4<<30)
	var ae *AdmissionError
	if !errors.As(err, &ae) || ae.PoolID != "gpu:0" || ae.Permanent || ae.Reason != "gpu-memory" {
		t.Fatalf("pending not counted: %v", err)
	}
	// Once resident, the sample shows its memory: only the sample counts.
	free = 4 << 30
	a.Resident()
	if _, err := p.Admit("image", 3<<30); err != nil {
		t.Errorf("after the first became resident: %v", err)
	}
	a.Release()
	// Bigger than the card: permanent.
	if _, err := p.Admit("llm", 13<<30); !errors.As(err, &ae) || !ae.Permanent {
		t.Errorf("larger than the GPU: %v", err)
	}
	// No GPU visible: refused with a reason.
	p.Sample = func() []GPUMemory { return nil }
	if _, err := p.Admit("llm", 1<<30); !errors.As(err, &ae) || ae.Reason != "measurement-unavailable" {
		t.Errorf("no sample: %v", err)
	}
}
