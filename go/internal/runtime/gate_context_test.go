package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

// holdingGate stands in for a gate whose launch is downloading what its model needs: it holds until its
// context ends. A call that isn't a launch has no context to end, and gets no target at once, with
// otherwise as its error.
func holdingGate(entered chan<- struct{}, otherwise error) Gate {
	return func(ctx context.Context, _ string) (*Target, error) {
		if ctx.Done() == nil {
			return nil, otherwise
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		case <-time.After(20 * time.Second):
			return nil, errors.New("the gate's context never ended")
		}
	}
}

// stopEndsTheGate starts a launch through acquire, waits until the gate holds it, stops the loads, and
// expects the launch to end promptly with an error.
func stopEndsTheGate(t *testing.T, acquire func() error, stop func(), entered <-chan struct{}) {
	t.Helper()
	ended := make(chan error, 1)
	go func() { ended <- acquire() }()
	select {
	case <-entered:
	case err := <-ended:
		t.Fatalf("the launch ended without giving the gate a context it could end: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the launch never gave the gate a context it could end")
	}
	stop()
	select {
	case err := <-ended:
		if err == nil {
			t.Error("a stopped launch reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the gate's work went on after the launch was stopped")
	}
}

// A media service's launch gives the gate its own context, which stopping the loads ends: whatever the
// gate is doing for it, a download included, ends too.
func TestAStoppedServiceLaunchEndsTheGatesWork(t *testing.T) {
	s, _ := newTestService(t)
	entered := make(chan struct{}, 1)
	// A launch that handed the gate no context gets an unknown model, and ends: the test says so.
	s.d.Gate = holdingGate(entered, ErrModelNotFound)
	stopEndsTheGate(t, func() error {
		l, err := s.Acquire(context.Background(), "a")
		if l != nil {
			l.Release()
		}
		return err
	}, s.StopLoads, entered)
}

// The same for the LLM.
func TestAStoppedLLMLaunchEndsTheGatesWork(t *testing.T) {
	m := newTestLLM(t, &bigHost{avail: 50 << 30})
	entered := make(chan struct{}, 1)
	// The LLM's admission check asks the gate with no context, and takes no target as no gate.
	m.d.Gate = holdingGate(entered, nil)
	stopEndsTheGate(t, func() error {
		l, err := m.Acquire(context.Background(), modelA)
		if l != nil {
			l.Release()
		}
		return err
	}, m.StopLoads, entered)
}
