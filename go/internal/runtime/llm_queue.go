package runtime

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/otlp"
)

type waiter struct {
	model       string
	parent      *otlp.Span // the request's span, for a start it triggers
	key         string
	ready       chan error
	lease       *Lease
	dispatching bool
	done        bool
	// dispatched is when the request left the queue: granted, or its own
	// model switch began (TS queueWaitMs excludes the activation).
	dispatched time.Time
}

// Lease is an admitted request. Release it when the response body completes.
type Lease struct {
	m       *LLM
	b       *backend
	once    sync.Once
	BaseURL string
	ModelID string
	Plan    *LLMPlan
	Spec    *catalog.Spec
	Waited  time.Duration
	// Active, Slots and Waiting describe the queue when the lease was granted.
	Active, Slots, Waiting int
}

func (l *Lease) Release() {
	l.once.Do(func() {
		l.m.mu.Lock()
		l.m.active--
		l.m.pump()
		l.m.mu.Unlock()
	})
}

// Acquire waits in the FIFO queue until the model is running and a slot is
// free. The queue deadline covers waiting and model transitions, not the
// backend start this request itself triggers.
func (m *LLM) Acquire(ctx context.Context, modelID string) (*Lease, error) {
	start := time.Now()
	key := m.selectionKey(modelID)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrUnavailable
	}
	if m.unloading == modelID {
		m.mu.Unlock()
		return nil, ErrUnloading
	}
	if len(m.queue) >= m.d.QueueCapacity {
		m.mu.Unlock()
		return nil, ErrQueueFull
	}
	w := &waiter{model: modelID, key: key, ready: make(chan error, 1), parent: otlp.FromContext(ctx)}
	m.queue = append(m.queue, w)
	m.pump()
	m.mu.Unlock()

	timer := time.NewTimer(m.d.QueueTimeout)
	defer timer.Stop()
	for {
		select {
		case err := <-w.ready:
			if err != nil {
				return nil, err
			}
			w.lease.Waited = w.dispatched.Sub(start)
			return w.lease, nil
		case <-ctx.Done():
			m.abandon(w)
			return nil, ctx.Err()
		case <-timer.C:
			m.mu.Lock()
			if w.dispatching || w.done {
				m.mu.Unlock()
				continue // its own backend start is not limited by the queue deadline
			}
			m.remove(w)
			m.pump()
			m.mu.Unlock()
			return nil, ErrQueueTimeout
		}
	}
}

func (m *LLM) remove(w *waiter) {
	if i := slices.Index(m.queue, w); i >= 0 {
		m.queue = slices.Delete(m.queue, i, i+1)
	}
}

// abandon drops a cancelled waiter, returning a lease it was already granted.
func (m *LLM) abandon(w *waiter) {
	m.mu.Lock()
	granted := w.done && w.lease != nil
	m.remove(w)
	m.pump()
	m.mu.Unlock()
	if granted {
		w.lease.Release()
	}
}

func (m *LLM) grant(w *waiter) {
	m.queue = m.queue[1:]
	m.active++
	if w.dispatched.IsZero() {
		w.dispatched = time.Now()
	}
	w.done = true
	w.lease = &Lease{m: m, b: m.cur, BaseURL: m.cur.baseURL, ModelID: m.cur.modelID, Plan: m.cur.plan, Spec: m.cur.spec,
		Active: m.active, Slots: m.cur.plan.Parallel.Slots, Waiting: len(m.queue)}
	w.ready <- nil
}

func (m *LLM) fail(w *waiter, err error) {
	m.remove(w)
	w.done = true
	w.ready <- err
}

// pump dispatches from the head of the queue. Called with mu held. Later
// requests never bypass a switch the head is waiting for.
func (m *LLM) pump() {
	for len(m.queue) > 0 && !m.starting && !m.claimed && !m.closed && m.unloading == "" {
		head := m.queue[0]
		if m.cur != nil && m.cur.modelID == head.model && m.cur.key == head.key {
			if m.active >= m.cur.plan.Parallel.Slots {
				return
			}
			m.grant(head)
			continue
		}
		if m.stopping {
			m.fail(head, ErrUnavailable)
			continue
		}
		if m.active > 0 {
			return // drain the current model first
		}
		m.starting = true
		head.dispatching = true
		head.dispatched = time.Now()
		m.switches.Add(1)
		ctx, cancel := context.WithCancelCause(context.Background())
		m.cancelSwitch = cancel
		go func(model, key string, parent *otlp.Span) {
			defer m.switches.Done()
			defer cancel(nil)
			m.switchTo(ctx, model, key, parent)
		}(head.model, head.key, head.parent)
		return
	}
}

// Interrupted reports whether the lease's runtime was stopped because memory
// turned critical; the request then answers 503 with Retry-After.
func (l *Lease) Interrupted() bool { return l.b != nil && l.b.emergency.Load() }
