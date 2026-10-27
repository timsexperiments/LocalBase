package memory

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"
)

// Reserve is a pool reserve: max(percent of capacity, minimum GiB).
type Reserve struct {
	Percent   float64
	MinimumGB float64
}

func (r Reserve) Bytes(capacity int64) int64 {
	return max(int64(math.Ceil(r.MinimumGB*float64(GiB))), int64(math.Floor(float64(capacity)*r.Percent/100)))
}

const warningReserveMultiplier = 1.25
const recoverySamples = 3

// AdmissionError is a rejected reservation. Reason matches the TS reasons:
// system-memory, accelerator-memory, memory-pressure, measurement-unavailable.
type AdmissionError struct {
	Reason         string
	PoolID         string
	RequestedBytes int64
	AvailableBytes int64
	ReserveBytes   int64
	PendingBytes   int64
	Confidence     string
	// Permanent: the demand can never fit on this host, even idle.
	Permanent bool
	// Diagnostics are the figures behind the refusal (nil when the error was
	// not made by admission).
	Diagnostics *AdmissionDiagnostics
}

// AdmissionDiagnostics are TS's RuntimeMemoryAdmissionError diagnostics.
// Byte figures of -1 are "unavailable" (the GPU working set is not sampled).
type AdmissionDiagnostics struct {
	MeasuredAvailable  int64 // the sample, crediting releasing runtimes
	Reserve            int64
	Pending            int64 // starting runtimes not yet in the sample
	Requested          int64
	EffectiveAvailable int64 // measured less pending
	CapturedAt         time.Time
	MeasuredPressure   string // the provider's own reading
	SafetyState        string // the hysteresis state
	RecoverySamples    int
	Confidence         string
}

// Attributes are the runtime.memory-admission-rejected attributes, in TS
// order (memoryAdmissionRejectedAttributes).
func (e *AdmissionError) Attributes() []any {
	attrs := []any{"reason", e.Reason, "pool", e.PoolID}
	g := e.Diagnostics
	if g == nil {
		return attrs
	}
	bytes := func(v int64) any {
		if v < 0 {
			return "unavailable"
		}
		return v
	}
	return append(attrs,
		"measured_available_bytes", bytes(g.MeasuredAvailable),
		"reserve_bytes", bytes(g.Reserve),
		"pending_bytes", bytes(g.Pending),
		"requested_bytes", bytes(g.Requested),
		"effective_available_bytes", bytes(g.EffectiveAvailable),
		"sample_captured_at_ms", g.CapturedAt.UnixMilli(),
		"sample_age_ms", max(0, time.Since(g.CapturedAt).Milliseconds()),
		"measured_pressure", g.MeasuredPressure,
		"safety_state", g.SafetyState,
		"recovery_samples", g.RecoverySamples,
		"demand_confidence", g.Confidence)
}

func (e *AdmissionError) Error() string {
	if e.Permanent {
		return fmt.Sprintf("model needs %.1f GiB but %s can hold at most %.1f GiB", gib(e.RequestedBytes), e.PoolID, gib(e.AvailableBytes))
	}
	return fmt.Sprintf("insufficient memory (%s): needs %.1f GiB, %.1f GiB available above the %.1f GiB reserve",
		e.Reason, gib(e.RequestedBytes), gib(e.AvailableBytes-e.ReserveBytes-e.PendingBytes), gib(e.ReserveBytes))
}

func gib(b int64) float64 { return float64(b) / float64(GiB) }

type reservation struct {
	projected   int64 // pool bytes (unified, or host on a host-only pool)
	accelerator int64
	resident    bool
	pid         int
}

// Controller reserves memory for runtimes before they start. It is the Go
// form of MemorySafetyController for a single system pool.
type Controller struct {
	host    HostProvider
	system  Reserve
	bypass  bool
	sampler func(ctx context.Context, pid int) (int64, bool)

	mu           sync.Mutex
	reservations map[string]*reservation
	state        string // healthy | constrained | critical
	normalRun    int
}

func NewController(host HostProvider, system Reserve, bypass bool) *Controller {
	return &Controller{host: host, system: system, bypass: bypass, sampler: Footprint,
		reservations: map[string]*reservation{}, state: "healthy"}
}

// SetSampler replaces the footprint sampler (tests).
func (c *Controller) SetSampler(f func(ctx context.Context, pid int) (int64, bool)) { c.sampler = f }

func (c *Controller) Topology() Topology { return c.host.Topology() }

func (c *Controller) projected(d Demand) int64 {
	if c.host.Topology().Unified {
		return d.UnifiedBytes
	}
	return d.HostBytes
}

// Reservation is held from admission until the runtime exits.
type Reservation struct {
	c  *Controller
	id string
	r  *reservation
}

// Materialize marks the runtime resident (its memory now shows in samples).
func (r *Reservation) Materialize(pid int) {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	if r.c.reservations[r.id] == r.r {
		r.r.resident, r.r.pid = true, pid
	}
}

// Bytes is the reserved pool projection (what releasing it frees from the
// admission ledger).
func (r *Reservation) Bytes() int64 {
	if r == nil || r.r == nil {
		return 0
	}
	return r.r.projected
}

func (r *Reservation) Release() {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	if r.c.reservations[r.id] == r.r {
		delete(r.c.reservations, r.id)
	}
}

// Reserve admits a runtime or returns *AdmissionError.
func (c *Controller) Reserve(ctx context.Context, id string, d Demand) (*Reservation, error) {
	snap := c.host.Snapshot(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.reservations[id]; exists {
		return nil, fmt.Errorf("runtime %q already has a memory reservation", id)
	}
	if !c.bypass {
		if err := c.evaluate(d, snap, nil, nil); err != nil {
			return nil, err
		}
	}
	r := &reservation{projected: c.projected(d), accelerator: d.AcceleratorBytes}
	c.reservations[id] = r
	return &Reservation{c: c, id: id, r: r}, nil
}

// CheckAdmission evaluates d as if the releasing runtimes had stopped,
// without reserving. A resident releasing runtime is credited at
// min(projection, sampled footprint) on unified hosts (#355): crediting the
// whole projection let a switch pass here, stop the current model, then fail.
func (c *Controller) CheckAdmission(ctx context.Context, d Demand, releasing []string) error {
	if c.bypass {
		return nil
	}
	rel := map[string]bool{}
	for _, id := range releasing {
		rel[id] = true
	}
	measured := map[string]int64{}
	if c.host.Topology().Unified && c.sampler != nil {
		c.mu.Lock()
		pids := map[string]int{}
		for id, r := range c.reservations {
			if rel[id] && r.resident && r.pid > 0 {
				pids[id] = r.pid
			}
		}
		c.mu.Unlock()
		for id, pid := range pids {
			if b, ok := c.sampler(ctx, pid); ok {
				measured[id] = b
			}
		}
	}
	snap := c.host.Snapshot(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.evaluate(d, snap, rel, measured)
}

func (c *Controller) evaluate(d Demand, snap Snapshot, releasing map[string]bool, measured map[string]int64) error {
	topo := c.host.Topology()
	req := c.projected(d)
	reserve := c.system.Bytes(topo.SystemCapacity)
	base := &AdmissionError{PoolID: "system", RequestedBytes: req, ReserveBytes: reserve, Confidence: d.Confidence}

	var pending, freed, resident int64
	for id, r := range c.reservations {
		if releasing[id] {
			if r.resident {
				credit := r.projected
				if m, ok := measured[id]; ok {
					credit = min(credit, m)
				}
				freed += credit
			}
			continue
		}
		resident += r.accelerator
		if !r.resident {
			pending += r.projected
		}
	}
	// reject attaches the figures behind the refusal (TS reject).
	reject := func(e AdmissionError) *AdmissionError {
		e.Diagnostics = c.diagnose(e.PoolID, d, snap, freed, pending, reserve)
		return &e
	}

	// Can never fit, even on an idle host.
	if topo.Unified && topo.GPUWorkingSetBytes > 0 && d.AcceleratorBytes > topo.GPUWorkingSetBytes {
		e := *base
		e.Reason, e.PoolID, e.RequestedBytes, e.AvailableBytes, e.Permanent = "accelerator-memory", "gpu-working-set", d.AcceleratorBytes, topo.GPUWorkingSetBytes, true
		return reject(e)
	}
	if topo.SystemCapacity > 0 && req > max(0, topo.SystemCapacity-reserve) {
		e := *base
		e.Reason, e.AvailableBytes, e.Permanent = "system-memory", max(0, topo.SystemCapacity-reserve), true
		return reject(e)
	}
	// The GPU working-set cap applies to the sum of loaded runtimes (#283).
	if topo.Unified && topo.GPUWorkingSetBytes > 0 && d.AcceleratorBytes > 0 && resident+d.AcceleratorBytes > topo.GPUWorkingSetBytes {
		e := *base
		e.Reason, e.PoolID, e.RequestedBytes, e.AvailableBytes = "accelerator-memory", "gpu-working-set", resident+d.AcceleratorBytes, topo.GPUWorkingSetBytes
		return reject(e)
	}
	if c.state != "healthy" {
		e := *base
		e.Reason = "memory-pressure"
		return reject(e)
	}
	if snap.Available < 0 || snap.Pressure == "unknown" {
		e := *base
		e.Reason = "measurement-unavailable"
		return reject(e)
	}
	avail := snap.Available + freed
	base.AvailableBytes, base.PendingBytes = avail, pending
	if snap.Pressure != "normal" || float64(avail) < float64(reserve)*warningReserveMultiplier {
		e := *base
		e.Reason = "memory-pressure"
		return reject(e)
	}
	if max(0, avail-pending)-req < reserve {
		e := *base
		e.Reason = "system-memory"
		return reject(e)
	}
	return nil
}

// diagnose computes TS's rejection diagnostics for a refusal on pool: the
// sample (crediting releasing runtimes), the pool's reserve, the pending
// demand and the request. The GPU working set is not a sampled pool.
func (c *Controller) diagnose(pool string, d Demand, snap Snapshot, freed, pending, reserve int64) *AdmissionDiagnostics {
	g := &AdmissionDiagnostics{MeasuredAvailable: -1, Reserve: -1, Pending: -1, Requested: c.projected(d),
		EffectiveAvailable: -1, CapturedAt: time.Now(), MeasuredPressure: "unknown",
		SafetyState: c.state, RecoverySamples: c.normalRun, Confidence: d.Confidence}
	if pool != "system" {
		g.Requested = d.AcceleratorBytes
		return g
	}
	g.Reserve, g.Pending, g.MeasuredPressure = reserve, pending, snap.Pressure
	if snap.Available >= 0 {
		g.MeasuredAvailable = snap.Available + freed
		g.EffectiveAvailable = max(0, g.MeasuredAvailable-pending)
	}
	return g
}

// Transition is the pressure state after a sample (TS MemorySafetyTransition):
// the hysteresis state (healthy, constrained or critical), the consecutive
// normal samples toward recovery, and the action (allow, constrain or
// emergency-stop).
type Transition struct {
	State         string
	NormalSamples int
	Action        string
}

// Poll samples the host and advances the pressure hysteresis.
func (c *Controller) Poll(ctx context.Context) Transition {
	if c.bypass {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.transition()
	}
	snap := c.host.Snapshot(ctx)
	reserve := c.system.Bytes(c.host.Topology().SystemCapacity)
	pressure := snap.Pressure
	if snap.Available >= 0 && pressure == "normal" {
		switch {
		case snap.Available < reserve:
			pressure = "critical"
		case float64(snap.Available) < float64(reserve)*warningReserveMultiplier:
			pressure = "constrained"
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch pressure {
	case "unknown":
		c.normalRun = 0
	case "critical":
		c.state, c.normalRun = "critical", 0
	case "constrained":
		if c.state != "critical" {
			c.state = "constrained"
		}
		c.normalRun = 0
	default:
		if c.state != "healthy" {
			c.normalRun++
			if c.normalRun >= recoverySamples {
				c.state, c.normalRun = "healthy", 0
			}
		}
	}
	return c.transition()
}

func (c *Controller) transition() Transition {
	t := Transition{State: c.state, NormalSamples: c.normalRun, Action: "allow"}
	switch c.state {
	case "critical":
		t.Action = "emergency-stop"
	case "constrained":
		t.Action = "constrain"
	}
	return t
}

// Snapshot reads current host memory (for display; admission reads its own).
func (c *Controller) Snapshot(ctx context.Context) Snapshot { return c.host.Snapshot(ctx) }

// Headroom is what the system pool could admit now, after the reserve and
// pending reservations, crediting the releasing runtimes as CheckAdmission
// does (placement plans the host side against it, DESIGN §20.3). It is -1
// when memory cannot be measured.
func (c *Controller) Headroom(ctx context.Context, releasing []string) int64 {
	snap := c.host.Snapshot(ctx)
	if snap.Available < 0 {
		return -1
	}
	rel := map[string]bool{}
	for _, id := range releasing {
		rel[id] = true
	}
	reserve := c.system.Bytes(c.host.Topology().SystemCapacity)
	c.mu.Lock()
	defer c.mu.Unlock()
	var freed, pending int64
	for id, r := range c.reservations {
		switch {
		case rel[id] && r.resident:
			freed += r.projected
		case !rel[id] && !r.resident:
			pending += r.projected
		}
	}
	return max(0, snap.Available+freed-pending-reserve)
}

// Reserved is the admission ledger: each runtime's reserved bytes (its pool
// projection), resident or not.
func (c *Controller) Reserved() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int64, len(c.reservations))
	for id, r := range c.reservations {
		out[id] = r.projected
	}
	return out
}
