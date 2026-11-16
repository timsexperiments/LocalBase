package runtime

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/otlp"
)

// switchTo replaces the running model. Preflight checks admission as if the
// current model were stopped, crediting it at most its measured footprint
// (#355), so a switch that cannot fit never stops the current model. If the
// new model fails after the old one stopped, the old one is restored (#296).
func (m *LLM) switchTo(ctx context.Context, modelID, key string, parent *otlp.Span) {
	err := m.d.Recovery.retryAfterRecovery(ctx, "llm", func() (memory.Demand, error) {
		return m.doSwitch(ctx, modelID, key, parent)
	})
	m.mu.Lock()
	defer m.mu.Unlock()
	m.starting = false
	m.cancelSwitch = nil
	m.failed = err != nil && m.cur == nil && ctx.Err() == nil // an eviction is not a failed start
	if err != nil {
		for len(m.queue) > 0 && m.queue[0].model == modelID && m.queue[0].key == key {
			m.fail(m.queue[0], err)
		}
	}
	m.pump()
}

func (m *LLM) doSwitch(ctx context.Context, modelID, key string, parent *otlp.Span) (demand memory.Demand, err error) {
	if m.d.BeforeSwitch != nil {
		m.d.BeforeSwitch()
	}
	// Catalog models keep the install check (and its consent error) first;
	// then the selection: an unselected or unavailable backend fails before
	// the current model is stopped.
	if m.d.Catalog.ByID(modelID) != nil {
		if err := m.ensureInstalled(ctx, modelID); err != nil {
			return demand, err
		}
	}
	t, err := m.target(ctx, modelID, true)
	if err != nil {
		return demand, err
	}
	if t == nil && m.d.Catalog.ByID(modelID) == nil {
		if err := m.ensureInstalled(ctx, modelID); err != nil {
			return demand, err
		}
	}
	plan, err := m.planFor(modelID, t)
	if err != nil {
		return demand, err
	}
	spec := m.d.Catalog.ByID(modelID)
	if t != nil && t.Imported {
		spec = nil
	}
	// The placement is resolved once per load and kept with it (DESIGN
	// §20.8 item 3): it does not follow free VRAM between requests.
	if err := m.place(modelID, plan, t); err != nil {
		return demand, err
	}
	demand = m.demandFor(modelID, plan, t)
	m.mu.Lock()
	prev := m.cur
	m.mu.Unlock()
	if prev != nil {
		if err := m.d.Memory.CheckAdmission(ctx, demand, []string{"llm"}); err != nil {
			if prev.modelID != modelID {
				logAdmissionRejected(m.d.Log, "llm", modalityComponents["llm"], err,
					"stage", "switch-preflight", "from_model", prev.modelID, "to_model", modelID)
			} else {
				logAdmissionRejected(m.d.Log, "llm", modalityComponents["llm"], err)
			}
			return demand, err
		}
		m.stopBackend(prev, StopSwitch)
		// Memory must settle before the new model starts; if it never does,
		// the previous model is restored (TS rollBackRejectedSwitch).
		if prev.modelID != modelID && ctx.Err() == nil {
			if err := settleAfterStop(ctx, m.d.Memory, demand, m.d.hooks); err != nil {
				if ctx.Err() != nil {
					return demand, context.Cause(ctx) // ended on purpose: nothing is restored
				}
				logRolledBack(m.d.Log, "llm", prev.modelID, modelID, true, err)
				m.restore(ctx, prev, parent)
				return demand, err
			}
		}
	}
	if ctx.Err() != nil {
		return demand, context.Cause(ctx)
	}
	b, err := m.startBackend(ctx, modelID, spec, plan, demand, t, parent)
	if ctx.Err() != nil && err != nil {
		return demand, context.Cause(ctx) // ended on purpose: nothing comes back
	}
	if err != nil {
		// A changed backend for the same model is not rolled back: the
		// person chose it, and the old one must not answer in its place.
		if prev != nil && prev.modelID != modelID {
			m.d.Log.Warn("Switch failed; restoring the previous model.", "event", "runtime.switch-rollback",
				"runtime", "llm", "model_id", modelID, "previous_model_id", prev.modelID, "error", err.Error())
			m.restore(ctx, prev, parent)
		}
		return demand, err
	}
	b.key = key
	if err := m.install(ctx, b); err != nil {
		return demand, err
	}
	// Only a chat model becomes the active LLM: a decision or embedding
	// request must not make the next default a model that rejects chat.
	if m.d.Activate != nil && (t == nil || !t.Imported) && m.isChat(modelID) {
		m.d.Activate("llm", modelID)
	}
	return demand, nil
}

// install makes a started backend current, unless its switch was ended meanwhile.
func (m *LLM) install(ctx context.Context, b *backend) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.stopBackend(b, StopShutdown)
		return ErrUnavailable
	}
	if ctx.Err() != nil {
		m.mu.Unlock()
		b.emergency.Store(true)
		m.stopBackend(b, StopMemory)
		return context.Cause(ctx)
	}
	m.cur = b
	m.mu.Unlock()
	m.watches.Add(1)
	go func() {
		defer m.watches.Done()
		m.watch(b)
	}()
	if b.spec != nil && m.d.Store != nil && (b.target == nil || b.target.Backend.Adapter == "llama-server/v1") {
		m.probes.Add(1)
		go func() {
			defer m.probes.Done()
			if rec, err := ProbeLlamaServer(context.Background(), m.d.Store, b.baseURL, b.spec); err != nil {
				m.d.Log.Debug("Runtime capability probe failed.", "runtime", "llm", "model_id", b.modelID, "error", err.Error())
			} else if rec != nil {
				m.d.Log.Info("Recorded runtime capabilities for "+b.modelID+" ("+rec.BuildInfo+").", "event", "runtime.capabilities-recorded", "category", "runtime", "runtime", "llm", "model_id", b.modelID)
			}
		}()
	}
	return nil
}

// NotInstalledError: the model's files are missing or incomplete and serve
// may not download them.
type NotInstalledError struct{ ModelID string }

func (e *NotInstalledError) Error() string {
	return fmt.Sprintf(`Model "%s" is not installed. Install it with "local-base models install %s" or restart serve with --install-missing.`, e.ModelID, e.ModelID)
}

// ensureInstalled checks the model's files (sizes, no hashing) before the
// current model is touched, and installs them when allowed.
func (m *LLM) ensureInstalled(ctx context.Context, modelID string) error {
	installed := false
	if spec := m.d.Catalog.ByID(modelID); spec != nil {
		installed, _ = spec.Installation(m.d.ModelsDir)
	} else {
		installed = fileExists(filepath.Join(m.d.ModelsDir, modelID+".bin")) || fileExists(filepath.Join(m.d.ModelsDir, modelID+".gguf"))
	}
	if installed {
		return nil
	}
	if m.d.Install == nil {
		return &NotInstalledError{ModelID: modelID}
	}
	return m.d.Install(ctx, modelID)
}

// restore starts the previous model again after a switch failed.
func (m *LLM) restore(ctx context.Context, prev *backend, parent *otlp.Span) {
	if ctx.Err() != nil {
		return // the switch was ended; a backend started now would be stopped at once
	}
	if rb, err := m.startBackend(ctx, prev.modelID, prev.spec, prev.plan, prev.demand, prev.target, parent); err == nil {
		rb.key = prev.key
		_ = m.install(ctx, rb) // its only refusal is an ended switch, which already reported
	}
}
