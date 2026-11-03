package obs

import (
	"testing"
	"time"
)

func TestSampler(t *testing.T) {
	s := &Sampler{Every: time.Minute, Burst: 3}
	t0 := time.Unix(1000, 0)
	for i := range 3 {
		if !s.Allow(t0.Add(time.Duration(i) * time.Second)) {
			t.Errorf("event %d was held back", i)
		}
	}
	for range 10 {
		if s.Allow(t0.Add(10 * time.Second)) {
			t.Error("past the burst")
		}
	}
	if n := s.Flush(t0.Add(30 * time.Second)); n != 0 {
		t.Errorf("flushed %d inside the interval", n)
	}
	if n := s.Flush(t0.Add(time.Minute)); n != 10 {
		t.Errorf("flushed %d, want 10", n)
	}
	if n := s.Flush(t0.Add(time.Minute)); n != 0 {
		t.Errorf("flushed %d twice", n)
	}
	if !s.Allow(t0.Add(time.Minute + time.Second)) {
		t.Error("a new interval starts over")
	}
	// An interval that passes with nothing held back has nothing to say.
	q := &Sampler{Every: time.Minute, Burst: 3}
	q.Allow(t0)
	if n := q.Flush(t0.Add(time.Hour)); n != 0 {
		t.Errorf("flushed %d", n)
	}
	// Allow alone moves to the next interval too, and carries what it held back to Flush.
	r := &Sampler{Every: time.Minute, Burst: 1}
	r.Allow(t0)
	r.Allow(t0)
	if !r.Allow(t0.Add(time.Hour)) {
		t.Error("a later interval")
	}
	if n := r.Flush(t0.Add(time.Hour)); n != 1 {
		t.Errorf("flushed %d, want 1", n)
	}
}
