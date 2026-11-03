package obs

import (
	"sync"
	"time"
)

// Sampler lets a few events through each interval and counts the rest, for what anyone can
// cause without a credential: enough to see it happening, without it writing the log.
type Sampler struct {
	Every time.Duration
	Burst int

	mu      sync.Mutex
	start   time.Time
	seen    int
	skipped int // in this interval
	unsaid  int // from intervals that have ended, until Flush says them
}

// roll starts a new interval once this one has ended.
func (s *Sampler) roll(now time.Time) {
	if s.start.IsZero() || now.Sub(s.start) >= s.Every {
		s.unsaid += s.skipped
		s.start, s.seen, s.skipped = now, 0, 0
	}
}

// Allow reports whether an event should be written.
func (s *Sampler) Allow(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roll(now)
	if s.seen < s.Burst {
		s.seen++
		return true
	}
	s.skipped++
	return false
}

// Flush returns how many were held back in intervals that have ended, once.
func (s *Sampler) Flush(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roll(now)
	n := s.unsaid
	s.unsaid = 0
	return n
}
