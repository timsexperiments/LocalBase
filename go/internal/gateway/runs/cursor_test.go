package runs

import "testing"

// Whatever goes wrong while reading events, the lock is not left taken.
func TestRunEventsAfterReleasesTheLock(t *testing.T) {
	r := &run{}
	func() {
		defer func() { _ = recover() }()
		r.eventsAfter(-1)
	}()
	if !r.mu.TryLock() {
		t.Fatal("the run's lock is still taken")
	}
	r.mu.Unlock()
}
