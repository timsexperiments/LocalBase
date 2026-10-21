package runtime

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

// Idle-eviction recovery (TS admitModelWithIdleRecovery with the
// reconciler's recoverWithIdleEviction and waitForAdmissionAfterEviction):
// when a model is refused for memory (not a permanent shortfall), the idle
// runtimes of the other modalities are claimed; if admission would pass with
// them stopped, they are stopped, the host is sampled until the model is
// admitted on uncredited memory (10 s, every 100 ms), and the model is
// started again. A retry that is refused for memory again answers the first
// refusal.

const (
	memorySettleTimeout  = 10 * time.Second       // TS MEMORY_SETTLE_TIMEOUT_MS
	memorySettleInterval = 100 * time.Millisecond // TS MEMORY_SETTLE_POLL_INTERVAL_MS
)

// idlePeer is a runtime that recovery may stop while it is idle.
type idlePeer interface {
	// runtimeID is the runtime's memory reservation id.
	runtimeID() string
	// claimIdle stops a running, idle runtime from taking work and reports
	// whether it did.
	claimIdle() bool
	// stopClaimed stops a claimed runtime; its next request starts it again.
	stopClaimed()
	// unclaim lets the runtime take work again.
	unclaim()
	// idle reports whether claimIdle would succeed now, without claiming.
	idle() bool
	// EmergencyStop rejects queued work with reason and stops the runtime,
	// active or not; its next request starts it again.
	EmergencyStop(reason error)
}

// IdleRecovery holds the runtimes recovery may stop. The zero value has no
// peers; Add registers them (the LLM manager and each Service).
type IdleRecovery struct {
	Memory *memory.Controller
	Log    *slog.Logger

	mu    sync.Mutex
	peers []idlePeer
	// lastEviction is when idle runtimes were last stopped for memory.
	lastEviction time.Time
	// settle overrides the settle timeout and interval (tests).
	settleTimeout, settleInterval time.Duration
}

// Add registers a runtime (*LLM or *Service) whose idle backend may be stopped.
func (r *IdleRecovery) Add(p idlePeer) {
	if r == nil || p == nil {
		return
	}
	r.mu.Lock()
	r.peers = append(r.peers, p)
	r.mu.Unlock()
}

// idleIDs are the runtimes other than modality that recovery could stop now.
func (r *IdleRecovery) idleIDs(modality string) []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	peers := append([]idlePeer(nil), r.peers...)
	r.mu.Unlock()
	var ids []string
	for _, p := range peers {
		if p.runtimeID() != modality && p.idle() {
			ids = append(ids, p.runtimeID())
		}
	}
	return ids
}

// recoverable reports whether err is a memory refusal recovery may resolve.
func recoverable(err error) bool {
	var ae *memory.AdmissionError
	return errors.As(err, &ae) && !ae.Permanent
}

// recover stops idle peers of modality when that makes demand admissible and
// waits for the host to show the memory. It reports whether a retry should
// follow.
func (r *IdleRecovery) recover(modality string, demand memory.Demand) bool {
	if r == nil || r.Memory == nil {
		return false
	}
	r.mu.Lock()
	peers := append([]idlePeer(nil), r.peers...)
	r.mu.Unlock()
	var claims []idlePeer
	defer func() {
		for _, p := range claims {
			p.unclaim()
		}
	}()
	ids := []string{}
	for _, p := range peers {
		if p.runtimeID() == modality || !p.claimIdle() {
			continue
		}
		claims = append(claims, p)
		ids = append(ids, p.runtimeID())
	}
	ctx := context.Background()
	if err := r.Memory.CheckAdmission(ctx, demand, ids); err != nil {
		return false
	}
	for _, p := range claims {
		if r.Log != nil {
			r.Log.Info("Stopping an idle runtime to make room.", "event", "runtime.idle-evicted",
				"runtime", p.runtimeID(), "for_runtime", modality)
		}
		p.stopClaimed()
	}
	if len(claims) > 0 {
		r.markEvicted()
	}
	timeout, interval := memorySettleTimeout, memorySettleInterval
	if r.settleTimeout > 0 {
		timeout, interval = r.settleTimeout, r.settleInterval
	}
	deadline := time.Now().Add(timeout)
	for {
		err := r.Memory.CheckAdmission(ctx, demand, nil)
		if err == nil {
			return true
		}
		if !recoverable(err) || time.Now().After(deadline) {
			return false
		}
		time.Sleep(min(interval, time.Until(deadline)+time.Millisecond))
	}
}

// retryAfterRecovery runs a switch, and once more after idle recovery when
// the first attempt was refused for memory. A retry refused for memory again
// answers the first refusal, as TS does.
func (r *IdleRecovery) retryAfterRecovery(modality string, attempt func() (memory.Demand, error)) error {
	demand, err := attempt()
	if !recoverable(err) || !r.recover(modality, demand) {
		return err
	}
	if _, retryErr := attempt(); !recoverable(retryErr) {
		return retryErr
	}
	return err
}

// --- peers ------------------------------------------------------------------------------

func (s *Service) runtimeID() string { return s.d.Modality }

func (s *Service) claimIdle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil || s.busy || s.claimed || s.closed || s.switching || len(s.queue) > 0 {
		return false
	}
	s.claimed = true
	return true
}

func (s *Service) stopClaimed() {
	s.mu.Lock()
	b := s.cur
	s.cur = nil
	s.mu.Unlock()
	if b != nil {
		s.stop(b)
	}
}

func (s *Service) idle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur != nil && !s.busy && !s.claimed && !s.closed && !s.switching && len(s.queue) == 0
}

func (s *Service) unclaim() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimed {
		s.claimed = false
		s.pump()
	}
}

func (m *LLM) runtimeID() string { return "llm" }

func (m *LLM) idle() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur != nil && m.active == 0 && !m.starting && !m.claimed && !m.closed && len(m.queue) == 0
}

func (m *LLM) claimIdle() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil || m.active > 0 || m.starting || m.claimed || m.closed || len(m.queue) > 0 {
		return false
	}
	m.claimed = true
	return true
}

func (m *LLM) stopClaimed() {
	m.mu.Lock()
	b := m.cur
	m.mu.Unlock()
	if b != nil {
		m.stopBackend(b)
	}
}

func (m *LLM) unclaim() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.claimed {
		m.claimed = false
		m.pump()
	}
}
