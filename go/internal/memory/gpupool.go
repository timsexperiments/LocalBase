package memory

import "sync"

// GPUPool is the gpu:0 admission pool for discrete NVIDIA placement (DESIGN
// §20.4, the minimum of §19.3): admit or refuse against the free VRAM
// sampled now, minus the reserve and the reservations not yet resident. It
// has no pressure states; the system pool keeps all of its safeguards.
type GPUPool struct {
	Reserve int64
	Sample  func() []GPUMemory // GPUMemoryNow by default

	mu      sync.Mutex
	pending map[*GPUReservation]int64
}

// GPUReservation is held from admission until the runtime stops.
type GPUReservation struct {
	p *GPUPool
}

// Admit reserves bytes on GPU 0 for runtime id, or returns *AdmissionError
// (pool gpu:0).
func (p *GPUPool) Admit(id string, bytes int64) (*GPUReservation, error) {
	sample := p.Sample
	if sample == nil {
		sample = GPUMemoryNow
	}
	gpus := sample()
	base := AdmissionError{PoolID: "gpu:0", RequestedBytes: bytes, ReserveBytes: p.Reserve, Confidence: "derived"}
	if len(gpus) == 0 {
		e := base
		e.Reason = "measurement-unavailable"
		return nil, &e
	}
	g := gpus[0]
	if bytes > g.TotalBytes-p.Reserve {
		e := base
		e.Reason, e.AvailableBytes, e.Permanent = "gpu-memory", max(0, g.TotalBytes-p.Reserve), true
		return nil, &e
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var pending int64
	for _, b := range p.pending {
		pending += b
	}
	if g.FreeBytes-pending-p.Reserve < bytes {
		e := base
		e.Reason, e.AvailableBytes, e.PendingBytes = "gpu-memory", g.FreeBytes, pending
		return nil, &e
	}
	if p.pending == nil {
		p.pending = map[*GPUReservation]int64{}
	}
	r := &GPUReservation{p: p}
	p.pending[r] = bytes
	return r, nil
}

// Resident marks the runtime loaded: its memory now shows in samples.
func (r *GPUReservation) Resident() {
	if r == nil {
		return
	}
	r.p.mu.Lock()
	delete(r.p.pending, r)
	r.p.mu.Unlock()
}

// Release drops the reservation.
func (r *GPUReservation) Release() { r.Resident() }
