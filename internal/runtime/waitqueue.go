package runtime

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/otlp"
)

// queued is a request waiting in a runtime's FIFO queue: the LLM's, granted a *Lease, or a
// service's, granted a *ServiceLease. The runtime's mu guards every field but ready.
type queued[L comparable] struct {
	model, key string
	parent     *otlp.Span // the request's span, for a start it triggers
	ready      chan error
	lease      L
	done       bool
	// dispatching: its own backend start is running. The start has its own budget, so the queue's
	// deadline no longer applies.
	dispatching bool
	// dispatched is when the request left the queue: granted, or its own model switch began (TS
	// queueWaitMs excludes the activation).
	dispatched time.Time
}

func newQueued[L comparable](ctx context.Context, model, key string) *queued[L] {
	return &queued[L]{model: model, key: key, ready: make(chan error, 1), parent: otlp.FromContext(ctx)}
}

// waitQueue is a runtime's FIFO of waiting requests. It has no lock of its own: the runtime's mu
// guards it, and every method but wait is called with mu held.
type waitQueue[L comparable] []*queued[L]

// remove drops w if it is still queued.
func (q *waitQueue[L]) remove(w *queued[L]) {
	if i := slices.Index(*q, w); i >= 0 {
		*q = slices.Delete(*q, i, i+1)
	}
}

// wait waits, without mu held, for w's answer: a lease, an error from the runtime, the request's
// own cancellation, or the queue deadline. A cancelled request gives back a lease it was already
// granted. The deadline doesn't apply once w's own backend start is running, or once it has its
// answer. pump runs with mu held after w leaves the queue, so the next request can go.
func (q *waitQueue[L]) wait(ctx context.Context, mu *sync.Mutex, w *queued[L], timeout time.Duration, pump func(), release func(L)) (L, error) {
	var none L
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case err := <-w.ready:
			if err != nil {
				return none, err
			}
			return w.lease, nil
		case <-ctx.Done():
			mu.Lock()
			granted := w.done && w.lease != none
			q.remove(w)
			pump()
			mu.Unlock()
			if granted {
				release(w.lease)
			}
			return none, ctx.Err()
		case <-timer.C:
			mu.Lock()
			if w.dispatching || w.done {
				mu.Unlock()
				continue // its own backend start is not limited by the queue deadline
			}
			q.remove(w)
			pump()
			mu.Unlock()
			return none, ErrQueueTimeout
		}
	}
}
