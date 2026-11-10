package gateway

import "testing"

// The flags reach the runs' options, and unset they leave them unset, for the defaults.
func TestRunMemoryLimitsAreConfigurable(t *testing.T) {
	s := &Server{cfg: Config{RunMemoryLimit: 5 << 20, RunsMemoryLimit: 7 << 20}}
	if o := s.runOptions(""); o.MaxHeld != 5<<20 || o.MaxHeldTotal != 7<<20 {
		t.Errorf("%d %d", o.MaxHeld, o.MaxHeldTotal)
	}
	if o := (&Server{}).runOptions(""); o.MaxHeld != 0 || o.MaxHeldTotal != 0 {
		t.Errorf("defaults: %d %d", o.MaxHeld, o.MaxHeldTotal)
	}
}
