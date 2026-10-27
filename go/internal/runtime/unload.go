package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Explicit unload (DESIGN §22): drain a loaded model like an eviction, then
// stop its backend and release its reservation. Nothing else changes: the
// model reloads on demand, and idle recovery and the default-model logic
// are untouched.

var (
	// ErrNotLoaded: the model is not the one running on this runtime.
	ErrNotLoaded = errors.New("model not loaded")
	// ErrUnloading: the model is draining for an explicit unload; a new
	// request for it is refused (transient: it reloads afterwards).
	ErrUnloading = errors.New("model unloading")
)

// BusyError is an unload that could not drain: requests still hold the
// model (or a switch is under way).
type BusyError struct {
	ModelID  string
	InFlight int
}

func (e *BusyError) Error() string {
	return fmt.Sprintf("%s has %d request(s) in flight", e.ModelID, e.InFlight)
}

// unloadPoll is how often a drain checks for the last lease.
const unloadPoll = 20 * time.Millisecond

// Unload drains modelID and stops it, waiting up to wait for in-flight
// requests (0: refuse at once if any). Queued requests for the model that
// have not started are refused with ErrUnloading, as are new ones until
// the drain ends. It returns the reserved bytes it freed.
func (m *LLM) Unload(ctx context.Context, modelID string, wait time.Duration) (int64, error) {
	m.mu.Lock()
	b := m.cur
	switch {
	case b == nil || b.modelID != modelID:
		m.mu.Unlock()
		return 0, ErrNotLoaded
	case m.starting || (m.unloading != "" && m.unloading != modelID):
		n := m.active
		m.mu.Unlock()
		return 0, &BusyError{ModelID: modelID, InFlight: n}
	case wait <= 0 && m.active > 0:
		n := m.active
		m.mu.Unlock()
		return 0, &BusyError{ModelID: modelID, InFlight: n}
	}
	m.unloading = modelID
	for _, w := range append([]*waiter(nil), m.queue...) {
		if w.model == modelID && !w.dispatching {
			m.fail(w, ErrUnloading)
		}
	}
	m.mu.Unlock()
	deadline := time.Now().Add(wait)
	for {
		m.mu.Lock()
		if m.cur != b {
			// Stopped meanwhile (memory, a crash): nothing left to unload.
			m.unloading = ""
			m.pump()
			m.mu.Unlock()
			return 0, nil
		}
		if m.active == 0 && !m.starting && !m.claimed {
			m.claimed = true
			m.mu.Unlock()
			freed := b.res.Bytes()
			m.stopBackend(b, StopUnload)
			m.mu.Lock()
			m.claimed, m.unloading = false, ""
			m.pump()
			m.mu.Unlock()
			return freed, nil
		}
		if n := m.active; time.Now().After(deadline) || ctx.Err() != nil {
			m.unloading = ""
			m.pump()
			m.mu.Unlock()
			return 0, &BusyError{ModelID: modelID, InFlight: n}
		}
		m.mu.Unlock()
		time.Sleep(unloadPoll)
	}
}

// Unload drains and stops the service's model; see LLM.Unload. A service
// runs one request at a time, so in flight is 0 or 1.
func (s *Service) Unload(ctx context.Context, modelID string, wait time.Duration) (int64, error) {
	s.mu.Lock()
	b := s.cur
	inFlight := func() int {
		if s.busy {
			return 1
		}
		return 0
	}
	switch {
	case b == nil || b.modelID != modelID:
		s.mu.Unlock()
		return 0, ErrNotLoaded
	case s.switching || (s.unloading != "" && s.unloading != modelID), wait <= 0 && s.busy:
		n := inFlight()
		s.mu.Unlock()
		return 0, &BusyError{ModelID: modelID, InFlight: n}
	}
	s.unloading = modelID
	for _, w := range append([]*svcWaiter(nil), s.queue...) {
		if w.model == modelID && !w.done {
			s.remove(w)
			w.done = true
			w.ready <- ErrUnloading
		}
	}
	s.mu.Unlock()
	deadline := time.Now().Add(wait)
	for {
		s.mu.Lock()
		if s.cur != b {
			s.unloading = ""
			s.pump()
			s.mu.Unlock()
			return 0, nil
		}
		if !s.busy && !s.switching && !s.claimed {
			s.cur, s.claimed = nil, true
			s.mu.Unlock()
			freed := b.res.Bytes()
			s.stop(b, StopUnload)
			s.mu.Lock()
			s.claimed, s.unloading = false, ""
			s.pump()
			s.mu.Unlock()
			return freed, nil
		}
		if n := inFlight(); time.Now().After(deadline) || ctx.Err() != nil {
			s.unloading = ""
			s.pump()
			s.mu.Unlock()
			return 0, &BusyError{ModelID: modelID, InFlight: n}
		}
		s.mu.Unlock()
		time.Sleep(unloadPoll)
	}
}
