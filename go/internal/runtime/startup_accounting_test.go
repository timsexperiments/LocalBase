package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

// A starting model has not shown up in the host's free memory yet, so its demand
// must still count. Started at spawn, two big models could each see the same free
// memory and both be admitted.
func TestStartingBackendStillCountsAsPending(t *testing.T) {
	host := newStepHost(50 * memory.GiB)
	mem := memory.NewController(unsampled{host}, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	m := newTestLLMWith(t, mem)
	t.Setenv("FAKE_STARTUP_MS", "1500")
	done := make(chan error, 1)
	go func() {
		l, err := m.Acquire(context.Background(), modelA)
		if err == nil {
			l.Release()
		}
		done <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for !m.Loading(modelA) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// Give the process time to spawn, inside its 1.5 s startup.
	time.Sleep(300 * time.Millisecond)
	m.mu.Lock()
	demand := m.d.Memory
	m.mu.Unlock()
	big := memory.Demand{UnifiedBytes: 36 * memory.GiB, HostBytes: 36 * memory.GiB, Confidence: "derived"}
	r, err := demand.Reserve(context.Background(), "peer", big)
	if err == nil {
		r.Release()
		t.Error("a second big start was admitted against memory the first has not loaded into yet")
	}
	var ae *memory.AdmissionError
	if err != nil && !errors.As(err, &ae) {
		t.Errorf("refusal: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Ready, and a fresh sample: the same request now judges the sample alone.
	r, err = demand.Reserve(context.Background(), "peer", big)
	if err != nil {
		t.Errorf("after the model is ready: %v", err)
	} else {
		r.Release()
	}
}
