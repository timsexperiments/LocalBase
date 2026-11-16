package runtime

import (
	"context"
	"slices"
	"sync"
	"testing"
)

// gateCalls records, for each call to a gate, whether it asked the gate to install.
type gateCalls struct {
	mu      sync.Mutex
	install []bool
}

func (c *gateCalls) record(g Gate) Gate {
	return func(ctx context.Context, id string, install bool) (*Target, error) {
		c.mu.Lock()
		c.install = append(c.install, install)
		c.mu.Unlock()
		return g(ctx, id, install)
	}
}

// take returns the calls since the last take.
func (c *gateCalls) take() []bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	got := c.install
	c.install = nil
	return got
}

// questions runs each question and expects it to ask the gate, never to install.
func (c *gateCalls) questions(t *testing.T, asks map[string]func()) {
	t.Helper()
	for name, ask := range asks {
		ask()
		if got := c.take(); len(got) == 0 || slices.Contains(got, true) {
			t.Errorf("%s asked the gate %v: a question asks without installing", name, got)
		}
	}
}

// launch runs a launch and expects it to ask the gate to install.
func (c *gateCalls) launch(t *testing.T, name string, run func()) {
	t.Helper()
	run()
	if got := c.take(); !slices.Contains(got, true) {
		t.Errorf("%s asked the gate %v: a launch asks it to install", name, got)
	}
}

// Only a launch may have the gate install what a model is missing. Resolving a model, planning it,
// previewing its admission and asking whether it fits now all ask the gate without, so none of them
// downloads anything.
func TestOnlyALaunchAsksTheGateToInstall(t *testing.T) {
	ctx := context.Background()
	t.Run("llm", func(t *testing.T) {
		m, _ := newPlacedLLM(t)
		var calls gateCalls
		m.d.Gate = calls.record(func(context.Context, string, bool) (*Target, error) { return nil, nil })
		calls.questions(t, map[string]func(){
			"Resolve":          func() { _, _, _ = m.Resolve("not-configured") },
			"Plan":             func() { _, _ = m.Plan(modelA) },
			"PreviewPlacement": func() { _, _ = m.PreviewPlacement(modelA, m.d.Placement.Request(modelA)) },
			"NextLoadBytes":    func() { _, _ = m.NextLoadBytes(modelA) },
			"fitsNow":          func() { _ = m.fitsNow(modelA, 0) },
			"PreviewAdmission": func() { _, _, _ = m.PreviewAdmission(ctx, modelA, false) },
		})
		calls.launch(t, "Acquire", func() {
			l, err := m.Acquire(ctx, modelA)
			if err != nil {
				t.Fatal(err)
			}
			l.Release()
		})
	})
	t.Run("media", func(t *testing.T) {
		s, _ := newTestService(t)
		var calls gateCalls
		s.d.Gate = calls.record(s.d.Gate)
		calls.questions(t, map[string]func(){
			"Resolve":          func() { _, _ = s.Resolve("not-configured") },
			"PreviewAdmission": func() { _, _, _ = s.PreviewAdmission(ctx, "a", false) },
		})
		calls.launch(t, "Acquire", func() {
			l, err := s.Acquire(ctx, "b")
			if err != nil {
				t.Fatal(err)
			}
			l.Release()
		})
		calls.launch(t, "reloadRestored", func() { s.reloadRestored(ctx, "a") })
	})
}
