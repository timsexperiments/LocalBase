package runtime

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/memory/memorytest"
)

// Close cancels a switch that is waiting for memory to settle, and does not wait out the settle or
// start the previous model again after it (review 04-M2). Before, Close waited the whole settle and
// then a restore that was abandoned at once.
func TestCloseDoesNotWaitOutAnLLMSettle(t *testing.T) {
	defer func(d, i time.Duration) { postStopSettle, postStopInterval = d, i }(postStopSettle, postStopInterval)
	postStopSettle, postStopInterval = 5*time.Second, 20*time.Millisecond
	host := newStepHost(50 * memory.GiB)
	mem := memory.NewController(memorytest.Unsampled{HostProvider: host}, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	m := newTestLLMWith(t, mem)
	var logs syncBuffer
	m.d.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	ctx := context.Background()
	l, err := m.Acquire(ctx, modelA)
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	host.avail.Store(11 * memory.GiB) // passes with A's credit, never settles
	switched := make(chan error, 1)
	go func() {
		_, err := m.Acquire(ctx, modelB)
		switched <- err
	}()
	time.Sleep(500 * time.Millisecond) // A has stopped and the settle is under way
	began := time.Now()
	m.Close()
	if took := time.Since(began); took > 1500*time.Millisecond {
		t.Errorf("Close took %v with a switch settling; it should cancel the settle", took)
	}
	select {
	case err := <-switched:
		if err == nil {
			t.Error("a switch cancelled by Close succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the switch never ended")
	}
	if id, _, _ := m.Running(); id != "" {
		t.Errorf("after Close, %q is running", id)
	}
}

// A media runtime's rolled-back model is reloaded in the background; Close stops that too, so
// nothing of it runs or logs after Close returns (review 04-M4).
func TestCloseEndsARestoredModelsReload(t *testing.T) {
	defer func(d, i time.Duration) { postStopSettle, postStopInterval = d, i }(postStopSettle, postStopInterval)
	postStopSettle, postStopInterval = 400*time.Millisecond, 20*time.Millisecond
	host := newStepHost(30 * memory.GiB)
	var logs syncBuffer
	s := newLoggedService(t, host, &logs)
	ctx := context.Background()
	l, err := s.Acquire(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	host.avail.Store(11*memory.GiB + memory.GiB/2) // the switch is rolled back, and memory stays short
	if _, err := s.Acquire(ctx, "b"); !errors.As(err, new(*memory.AdmissionError)) {
		t.Fatalf("switch: %v", err)
	}
	s.Close() // the reload of "a" is now waiting for memory
	closedAt := len(logs.String())
	time.Sleep(3 * postStopSettle)
	if after := logs.String()[closedAt:]; strings.TrimSpace(after) != "" {
		t.Errorf("logged after Close returned:\n%s", after)
	}
}
