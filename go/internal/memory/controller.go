package memory

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
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
	// ReservedBytes is what LocalBase's other runtimes have reserved: a promise, not a measurement.
	ReservedBytes int64
	Confidence    string
	// Permanent: the demand can never fit on this host, even idle.
	Permanent bool
	// CapacityBytes is the pool's total when Permanent; AvailableBytes is then what
	// the pool can ever give it, after the reserve.
	CapacityBytes int64
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
	// residentAt is the sample epoch it became resident in: a sample that began
	// earlier cannot show its memory.
	residentAt uint64
}

// Controller reserves memory for runtimes before they start. It is the Go
// form of MemorySafetyController for a single system pool.
type Controller struct {
	host    HostProvider
	system  Reserve
	bypass  bool
	sampler func(ctx context.Context, pid int) (int64, bool)

	viewMu   sync.Mutex
	lastView *view

	mu           sync.Mutex
	reservations map[string]*reservation
	epoch        uint64 // advanced when a runtime becomes resident
	state        string // healthy | constrained | critical
	normalRun    int
}

// FootprintSampler is a HostProvider that measures its processes' footprints itself (tests: fixed
// figures, or none). A Controller measures a host without it with Footprint.
type FootprintSampler interface {
	Footprint(ctx context.Context, pid int) (int64, bool)
}

// NewController is the controller of host's memory. Its sampler is fixed here, from host, so it
// can't change while admission reads it.
func NewController(host HostProvider, system Reserve, bypass bool) *Controller {
	sampler := Footprint
	if s, ok := host.(FootprintSampler); ok {
		sampler = s.Footprint
	}
	return &Controller{host: host, system: system, bypass: bypass, sampler: sampler,
		reservations: map[string]*reservation{}, state: "healthy"}
}

func (c *Controller) Topology() Topology { return c.host.Topology() }

// Projected is what a demand counts for against this host's pool.
func (c *Controller) Projected(d Demand) int64 { return c.projected(d) }

// ReserveBytes is the memory kept for the rest of the computer.
func (c *Controller) ReserveBytes() int64 { return c.system.Bytes(c.host.Topology().SystemCapacity) }

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

// Materialize marks the runtime resident: loaded, so its memory shows in samples
// taken from now on. Until then its demand counts as pending. Call it when the
// backend is ready, not when it is spawned: a spawned process has not loaded yet.
func (r *Reservation) Materialize(pid int) {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	if r.c.reservations[r.id] == r.r {
		r.c.epoch++
		r.r.resident, r.r.pid, r.r.residentAt = true, pid, r.c.epoch
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
	seen := c.sampleEpoch()
	snap := c.host.Snapshot(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.reservations[id]; exists {
		return nil, fmt.Errorf("runtime %q already has a memory reservation", id)
	}
	if !c.bypass {
		if err := c.evaluate(d, snap, nil, nil, seen); err != nil {
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
	seen := c.sampleEpoch()
	snap := c.host.Snapshot(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.evaluate(d, snap, rel, measured, seen)
}

// sampleEpoch is read before a sample is taken: runtimes resident by this epoch
// show in that sample.
func (c *Controller) sampleEpoch() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.epoch
}

// ledger totals the reservations as evaluate counts them, with c.mu held. seen is
// the epoch of the sample they are judged against.
func (c *Controller) ledger(releasing map[string]bool, measured map[string]int64, seen uint64) (pending, freed, resident int64) {
	for id, r := range c.reservations {
		inSample := r.resident && r.residentAt <= seen
		if releasing[id] {
			if inSample {
				credit := r.projected
				if m, ok := measured[id]; ok {
					credit = min(credit, m)
				}
				freed += credit
			}
			continue
		}
		resident += r.accelerator
		if !inSample {
			pending += r.projected
		}
	}
	return pending, freed, resident
}

func (c *Controller) evaluate(d Demand, snap Snapshot, releasing map[string]bool, measured map[string]int64, seen uint64) error {
	topo := c.host.Topology()
	req := c.projected(d)
	reserve := c.system.Bytes(topo.SystemCapacity)
	base := &AdmissionError{PoolID: "system", RequestedBytes: req, ReserveBytes: reserve, Confidence: d.Confidence}

	pending, freed, resident := c.ledger(releasing, measured, seen)
	// reject attaches the figures behind the refusal (TS reject).
	reject := func(e AdmissionError) *AdmissionError {
		e.Diagnostics = c.diagnose(e.PoolID, d, snap, freed, pending, reserve)
		for id, r := range c.reservations {
			if !releasing[id] {
				e.ReservedBytes += r.projected
			}
		}
		return &e
	}

	// Can never fit, even on an idle host.
	if topo.Unified && topo.GPUWorkingSetBytes > 0 && d.AcceleratorBytes > topo.GPUWorkingSetBytes {
		e := *base
		e.Reason, e.PoolID, e.RequestedBytes, e.AvailableBytes, e.Permanent = "accelerator-memory", "gpu-working-set", d.AcceleratorBytes, topo.GPUWorkingSetBytes, true
		e.CapacityBytes = topo.GPUWorkingSetBytes
		return reject(e)
	}
	if topo.SystemCapacity > 0 && req > max(0, topo.SystemCapacity-reserve) {
		e := *base
		e.Reason, e.AvailableBytes, e.Permanent = "system-memory", max(0, topo.SystemCapacity-reserve), true
		e.CapacityBytes = topo.SystemCapacity
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

// Snapshot reads host memory for display, at most once a second however many
// pages poll it (it may spawn a process). Admission reads its own, always fresh.
func (c *Controller) Snapshot(ctx context.Context) Snapshot { return c.reading(ctx).snap }

// viewTTL bounds how often display reads sample the host.
const viewTTL = time.Second

// view is one shared display reading of the host and resident runtimes.
type view struct {
	at       time.Time
	snap     Snapshot
	measured map[string]int64
	seen     uint64 // the epoch this sample began in
}

func (c *Controller) reading(ctx context.Context) *view {
	c.viewMu.Lock()
	defer c.viewMu.Unlock()
	if c.lastView != nil && time.Since(c.lastView.at) < viewTTL {
		return c.lastView
	}
	seen := c.sampleEpoch()
	v := &view{snap: c.host.Snapshot(ctx), measured: map[string]int64{}, seen: seen}
	if c.host.Topology().Unified && c.sampler != nil {
		c.mu.Lock()
		pids := map[string]int{}
		for id, r := range c.reservations {
			if r.resident && r.pid > 0 {
				pids[id] = r.pid
			}
		}
		c.mu.Unlock()
		for id, pid := range pids {
			if b, ok := c.sampler(ctx, pid); ok {
				v.measured[id] = b
			}
		}
	}
	v.at = time.Now()
	c.lastView = v
	return v
}

// RuntimeMemory is what one runtime holds. MeasuredBytes is -1 when there is no
// reading: it is starting, or this platform cannot sample it.
type RuntimeMemory struct {
	Runtime       string
	PlannedBytes  int64
	MeasuredBytes int64
}

// Runtimes lists every reservation, by runtime, from the shared reading.
func (c *Controller) Runtimes(ctx context.Context) []RuntimeMemory {
	v := c.reading(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]RuntimeMemory, 0, len(c.reservations))
	for id, r := range c.reservations {
		m := RuntimeMemory{Runtime: id, PlannedBytes: r.projected, MeasuredBytes: -1}
		if b, ok := v.measured[id]; ok && r.resident {
			m.MeasuredBytes = b
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Runtime < out[j].Runtime })
	return out
}

// PressureState is the state admission refuses on (healthy, constrained or
// critical): the host's reading compared with the reserve, with hysteresis.
func (c *Controller) PressureState() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// Settling reports memory that has recovered while admission still refuses for pressure:
// the state is not healthy and normal samples are counting toward recovery. It ends when
// the state does, so refusals for pressure may happen for as long as it is true.
func (c *Controller) Settling() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state != "healthy" && c.normalRun > 0
}

// AdmissionPreview is what admitting a demand would decide now, changing nothing.
type AdmissionPreview struct {
	RequestedBytes int64
	ReserveBytes   int64
	// AvailableBytes counts what the releasing runtimes would free, less pending
	// reservations; -1 when memory cannot be measured.
	AvailableBytes int64
	// PoolID is the pool a refusal was decided on; "system" when admitted.
	PoolID string
	Err    error
}

// Preview runs the same evaluate as a real admission on the shared reading, so
// its verdict and figures cannot drift from a refusal's.
func (c *Controller) Preview(ctx context.Context, d Demand, releasing []string) AdmissionPreview {
	v := c.reading(ctx)
	rel := map[string]bool{}
	for _, id := range releasing {
		rel[id] = true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	reserve := c.system.Bytes(c.host.Topology().SystemCapacity)
	p := AdmissionPreview{RequestedBytes: c.projected(d), ReserveBytes: reserve, AvailableBytes: -1, PoolID: "system"}
	if v.snap.Available >= 0 {
		pending, freed, _ := c.ledger(rel, v.measured, v.seen)
		p.AvailableBytes = max(0, v.snap.Available+freed-pending)
	}
	if !c.bypass {
		if err := c.evaluate(d, v.snap, rel, v.measured, v.seen); err != nil {
			p.Err = err
			// A refusal's own figures: a GPU refusal counts accelerator bytes
			// against the GPU limit, not the system pool.
			var ae *AdmissionError
			if errors.As(err, &ae) {
				p.RequestedBytes, p.PoolID = ae.RequestedBytes, ae.PoolID
				if ae.Permanent || ae.AvailableBytes > 0 {
					p.AvailableBytes = ae.AvailableBytes
				}
			}
		}
	}
	return p
}

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
