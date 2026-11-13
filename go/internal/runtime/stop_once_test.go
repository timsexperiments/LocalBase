package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

// gatedHost holds the next Snapshot after arm until open, so a test can stop a
// model switch in its preflight, after it captured the backend it will stop.
type gatedHost struct {
	*stepHost
	armed   atomic.Bool
	reached chan struct{}
	open    chan struct{}
}

func (h *gatedHost) arm() { h.armed.Store(true) }

func (h *gatedHost) Snapshot(ctx context.Context) memory.Snapshot {
	if h.armed.CompareAndSwap(true, false) {
		close(h.reached)
		<-h.open
	}
	return h.stepHost.Snapshot(ctx)
}

// A switch and a memory eviction can capture the same backend. Each stops it;
// the second must not close its channel again, which panics on a goroutine no
// HTTP recovery covers and takes the whole gateway down.
func TestSwitchAndEmergencyStopSameBackend(t *testing.T) {
	host := &gatedHost{stepHost: newStepHost(50 * memory.GiB), reached: make(chan struct{}), open: make(chan struct{})}
	mem := memory.NewController(unsampled{host}, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	m := newTestLLMWith(t, mem)
	var logs syncBuffer
	m.d.Log = slog.New(slog.NewJSONHandler(&logs, nil))

	l, err := m.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	l.Release()

	host.arm()
	switched := make(chan error, 1)
	go func() {
		l, err := m.Acquire(context.Background(), modelB)
		if err == nil {
			l.Release()
		}
		switched <- err
	}()
	select {
	case <-host.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("the switch never reached its preflight")
	}
	// The switch holds the backend for model A. The eviction takes it too.
	m.EmergencyStop(errors.New("critical"))
	close(host.open)
	select {
	case err := <-switched:
		// Either outcome is fine: the eviction rejects queued work, and the
		// waiter may still count as queued while its switch runs.
		if err != nil && err.Error() != "critical" {
			t.Fatalf("switch after the eviction: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the switch never finished")
	}
	if got := stopsOf(logs.String(), modelA); got != "memory" {
		t.Errorf("model A's stops: %q, want one, for memory", got)
	}
}

// A stop that captured the backend before it crashed finds it already over.
func TestStopAfterCrashReportsOnce(t *testing.T) {
	m := newTestLLM(t, &bigHost{avail: 50 * memory.GiB})
	var logs syncBuffer
	m.d.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	l, err := m.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	m.mu.Lock()
	b := m.cur
	m.mu.Unlock()
	syscall.Kill(b.proc.PID(), syscall.SIGKILL)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && stopsOf(logs.String(), modelA) == "" {
		time.Sleep(10 * time.Millisecond)
	}
	m.stopBackend(b, StopSwitch)
	if got := stopsOf(logs.String(), modelA); got != "crash" {
		t.Errorf("stops: %q, want one, for the crash", got)
	}
}

// stopsOf lists the reasons of a model's stop events, in order.
func stopsOf(logs, model string) string {
	var out []string
	for _, line := range strings.Split(logs, "\n") {
		var e struct {
			Event   string `json:"event"`
			ModelID string `json:"model_id"`
			Reason  string `json:"reason"`
		}
		if json.Unmarshal([]byte(line), &e) == nil && e.ModelID == model &&
			(e.Event == "runtime.stopped" || e.Event == "runtime.evicted") {
			out = append(out, e.Reason)
		}
	}
	return strings.Join(out, ",")
}
