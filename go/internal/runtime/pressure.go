package runtime

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

// Pressure-driven runtime stops (TS MemoryPressureMonitor with serve's
// applyElevatedMemoryPressure and reportMemoryPressureTransition). Host
// memory is sampled every 2 s without overlapping samples. While the
// hysteresis state is constrained, every sample stops the idle runtimes
// (requests in flight and queued work keep theirs); when nothing was idle on a
// fresh constrained sample outside the reclaim grace, active video jobs fail.
// On entering critical, video jobs fail, queued work is rejected and every
// runtime stops, active or not. Each state change logs
// runtime.memory-pressure-changed.

// PressurePollInterval is TS memoryPressurePollIntervalMs.
const PressurePollInterval = 2 * time.Second

// pressureTarget is what the monitor acts on: the runtimes recovery knows.
type pressureTarget interface {
	// evictIdle stops every running, idle runtime and returns how many.
	evictIdle() int
	// evictAll rejects queued work with reason and stops every runtime.
	evictAll(reason error)
	// withinReclaimGrace reports whether a memory eviction happened within
	// the settle timeout (TS isWithinReclaimGrace).
	withinReclaimGrace() bool
}

// PressureMonitor polls host memory and stops runtimes under pressure.
type PressureMonitor struct {
	memory *memory.Controller
	target pressureTarget
	log    *slog.Logger
	// FailVideo fails active video jobs (TS failActiveForMemoryPressure).
	FailVideo func()

	mu   sync.Mutex
	last string // the last applied state
	// lastTransition is the latest sample's transition.
	lastTransition memory.Transition
}

// NewPressureMonitor watches mem and acts on the runtimes in recovery.
func NewPressureMonitor(mem *memory.Controller, recovery *IdleRecovery, log *slog.Logger) *PressureMonitor {
	return &PressureMonitor{memory: mem, target: recovery, log: log, last: "healthy"}
}

// Run polls at once and then every interval until stop closes.
func (p *PressureMonitor) Run(stop <-chan struct{}, interval time.Duration) {
	p.Poll()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			p.Poll()
		}
	}
}

// Poll takes one sample and applies it.
func (p *PressureMonitor) Poll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.memory.Poll(context.Background())
	p.lastTransition = t
	previous := p.last
	changed := previous != t.State
	if t.State == "constrained" || (t.State == "critical" && changed) {
		p.applyElevated(t)
	}
	if changed {
		p.last = t.State
		p.report(previous, t.State)
	}
}

func (p *PressureMonitor) applyElevated(t memory.Transition) {
	switch t.State {
	case "constrained":
		if p.target.evictIdle() == 0 && t.NormalSamples == 0 && !p.target.withinReclaimGrace() {
			p.failVideo()
		}
	case "critical":
		p.failVideo()
		p.target.evictAll(ErrMemoryEmergency)
	}
}

func (p *PressureMonitor) failVideo() {
	if p.FailVideo != nil {
		p.FailVideo()
	}
}

func (p *PressureMonitor) report(previous, current string) {
	if p.log == nil {
		return
	}
	level := slog.LevelWarn
	switch current {
	case "healthy":
		level = slog.LevelInfo
	case "critical":
		level = slog.LevelError
	}
	p.log.Log(context.Background(), level, "Runtime memory pressure changed.", "event", "runtime.memory-pressure-changed",
		"category", "runtime", "component", "memory-safety", "runtime", "gateway",
		slog.Group("attributes", "previous_state", previous, "current_state", current))
}

// --- IdleRecovery as the pressure target ------------------------------------------------

func (r *IdleRecovery) evictIdle() int {
	r.mu.Lock()
	peers := append([]idlePeer(nil), r.peers...)
	r.mu.Unlock()
	n := 0
	for _, p := range peers {
		if !p.claimIdle() {
			continue
		}
		p.stopClaimed()
		p.unclaim()
		n++
	}
	if n > 0 {
		r.markEvicted()
	}
	return n
}

func (r *IdleRecovery) evictAll(reason error) {
	r.mu.Lock()
	peers := append([]idlePeer(nil), r.peers...)
	r.mu.Unlock()
	var wg sync.WaitGroup
	for _, p := range peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.EmergencyStop(reason)
		}()
	}
	wg.Wait()
}

func (r *IdleRecovery) markEvicted() {
	r.mu.Lock()
	r.lastEviction = time.Now()
	r.mu.Unlock()
}

func (r *IdleRecovery) withinReclaimGrace() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.lastEviction.IsZero() && time.Since(r.lastEviction) < memorySettleTimeout
}
