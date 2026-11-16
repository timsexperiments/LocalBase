package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type testLease struct{ released int }

// queueOf is a queue holding w, with the counters wait's callbacks bump.
func queueOf(w *queued[*testLease]) (*waitQueue[*testLease], *sync.Mutex, *int) {
	q := waitQueue[*testLease]{w}
	return &q, &sync.Mutex{}, new(int)
}

func TestAWaiterPastTheDeadlineLeavesTheQueue(t *testing.T) {
	w := newQueued[*testLease](context.Background(), "m", "")
	q, mu, pumps := queueOf(w)
	_, err := q.wait(context.Background(), mu, w, 10*time.Millisecond, func() { *pumps++ }, func(*testLease) { t.Error("released") })
	if !errors.Is(err, ErrQueueTimeout) || len(*q) != 0 || *pumps != 1 {
		t.Errorf("err %v, queue %d, pumps %d", err, len(*q), *pumps)
	}
}

func TestADeadlineDoesNotCutOffARequestsOwnStart(t *testing.T) {
	w := newQueued[*testLease](context.Background(), "m", "")
	q, mu, pumps := queueOf(w)
	mu.Lock()
	w.dispatching = true
	mu.Unlock()
	lease := &testLease{}
	go func() {
		time.Sleep(50 * time.Millisecond) // well past the deadline
		mu.Lock()
		*q = (*q)[1:]
		w.done, w.lease = true, lease
		mu.Unlock()
		w.ready <- nil
	}()
	got, err := q.wait(context.Background(), mu, w, 5*time.Millisecond, func() { *pumps++ }, func(*testLease) { t.Error("released") })
	if err != nil || got != lease || *pumps != 0 {
		t.Errorf("lease %v, err %v, pumps %d", got, err, *pumps)
	}
}

func TestACancelledRequestGivesBackALeaseItWasGranted(t *testing.T) {
	for _, granted := range []bool{false, true} {
		w := newQueued[*testLease](context.Background(), "m", "")
		q, mu, pumps := queueOf(w)
		lease := &testLease{}
		if granted {
			// Granted, its answer not yet read. ready is left empty so that the cancellation is the
			// only case wait's select can take.
			*q = nil
			w.done, w.lease = true, lease
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got, err := q.wait(ctx, mu, w, time.Minute, func() { *pumps++ }, func(l *testLease) { l.released++ })
		want := 0
		if granted {
			want = 1
		}
		if !errors.Is(err, context.Canceled) || got != nil || len(*q) != 0 || *pumps != 1 || lease.released != want {
			t.Errorf("granted %v: lease %v, err %v, queue %d, pumps %d, released %d", granted, got, err, len(*q), *pumps, lease.released)
		}
	}
}

func TestARuntimesErrorReachesTheRequest(t *testing.T) {
	w := newQueued[*testLease](context.Background(), "m", "")
	q, mu, pumps := queueOf(w)
	want := errors.New("start failed")
	*q = nil
	w.done = true
	w.ready <- want
	if _, err := q.wait(context.Background(), mu, w, time.Minute, func() { *pumps++ }, nil); !errors.Is(err, want) || *pumps != 0 {
		t.Errorf("err %v, pumps %d", err, *pumps)
	}
}

func TestRemoveDropsOnlyTheWaiterNamed(t *testing.T) {
	a, b, c := newQueued[*testLease](context.Background(), "a", ""), newQueued[*testLease](context.Background(), "b", ""), newQueued[*testLease](context.Background(), "c", "")
	q := waitQueue[*testLease]{a, b, c}
	q.remove(b)
	q.remove(b) // no longer there: nothing happens
	if len(q) != 2 || q[0] != a || q[1] != c {
		t.Errorf("%v", q)
	}
}
