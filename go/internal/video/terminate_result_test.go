package video

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// Two cancels at once: the second waits for the first's stop and hears how it went. A stop that failed
// leaves the runtime possibly running, so neither cancel may report it stopped.
func TestASecondCancelHearsHowTheStopWent(t *testing.T) {
	m, released := newTestManager(t, Options{})
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	acquire := func(context.Context) (Admission, error) { return noopAdmission{released}, nil }
	j, done, err := m.Start("o", map[string]any{"prompt": "slow"}, time.Minute, acquire, func() error {
		if calls.Add(1) > 1 {
			return nil
		}
		close(entered)
		<-release
		return errors.New("still running")
	})
	if err != nil {
		t.Fatal(err)
	}
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { _, _, err := m.Cancel("o", j.ID); first <- err }()
	<-entered
	go func() { _, _, err := m.Cancel("o", j.ID); second <- err }()
	select {
	case err := <-second:
		t.Fatalf("the second cancel returned while the stop was still running: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-first; err == nil {
		t.Error("the first cancel reported a failed stop as success")
	}
	if err := <-second; err == nil {
		t.Error("the second cancel reported a failed stop as success")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("the second cancel stopped the runtime itself (%d stops), not waiting for the first", n)
	}
	if got, _ := m.Get("o", j.ID); got.State == Cancelled {
		t.Fatal("cancelled while its runtime may still run")
	}
	if _, _, err := m.Cancel("o", j.ID); err != nil {
		t.Fatal(err)
	}
	if got := wait(t, done); got.State != Cancelled {
		t.Fatalf("retry %+v", got)
	}
}
