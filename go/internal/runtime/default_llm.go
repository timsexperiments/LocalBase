package runtime

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

// The fit-aware default chat model (Go only, DESIGN §18). With an ordered
// list of two or more defaults (store.Config.DefaultLLMs), a request that
// names no model, and a new browser conversation, get the first default
// that passes memory admission now: the same CheckAdmission the switch
// preflight runs, crediting the LLM slot (switch credit) and every idle
// runtime idle recovery could stop, without stopping anything. When none
// fits, the last default is used and gets the normal admission error.
//
// A request that names a model is never substituted. The choice changes
// only at decision points (a new conversation, or a request while the LLM
// is idle), after a minimum dwell; moving back up the list needs the earlier
// model to fit by a margin at upgradeSamples decision points in a row, like
// the pressure monitor's recovery count.

const (
	defaultDwell   = 30 * time.Second
	upgradeSamples = 3 // memory recoverySamples
	upgradeMargin  = 1 << 30
)

// DefaultChoice is the effective default chat model.
type DefaultChoice struct {
	ModelID   string `json:"modelId"`
	Preferred string `json:"preferredModelId"`
	// Reason says, in product words, why the preferred model is not used
	// ("" when it is).
	Reason string `json:"reason,omitempty"`
}

type defaultChooser struct {
	mu     sync.Mutex
	list   []string
	cur    string
	reason string
	since  time.Time
	streak int
	now    func() time.Time // tests
}

func (m *LLM) clock() time.Time {
	if m.chooser.now != nil {
		return m.chooser.now()
	}
	return time.Now()
}

// DefaultModel is the effective default chat model. decision marks a
// decision point (a new conversation, or a request that names no model);
// otherwise the last choice is reported, choosing only if none was made.
func (m *LLM) DefaultModel(decision bool) DefaultChoice {
	cfg := m.Config()
	list := cfg.DefaultLLMList()
	if len(list) < 2 {
		id := m.chatDefault(cfg.Active["llm"], cfg.Selected["llm"])
		return DefaultChoice{ModelID: id, Preferred: id}
	}
	c := &m.chooser
	c.mu.Lock()
	defer c.mu.Unlock()
	if !slices.Equal(c.list, list) {
		c.list, c.cur, c.reason, c.streak = slices.Clone(list), "", "", 0
	}
	now := m.clock()
	keep := c.cur != "" && slices.Contains(list, c.cur) &&
		(!decision || m.busy() || now.Sub(c.since) < defaultDwell)
	if keep {
		return DefaultChoice{ModelID: c.cur, Preferred: list[0], Reason: c.reason}
	}
	prev := c.cur
	chosen, reason, cause := m.chooseDefault(list, prev)
	if chosen != prev {
		c.since = now
		m.d.Log.Info("Default chat model selected.", "event", "model.default-selected",
			"preferred_model_id", list[0], "model_id", chosen, "previous_model_id", prev, "reason", cause)
	}
	c.cur, c.reason = chosen, reason
	return DefaultChoice{ModelID: chosen, Preferred: list[0], Reason: reason}
}

// chooseDefault applies the choice rule with hysteresis. Called with the
// chooser locked. cause is the admission outcome for the log.
func (m *LLM) chooseDefault(list []string, cur string) (chosen, reason, cause string) {
	c := &m.chooser
	first, why := -1, map[string]error{}
	for i, id := range list {
		if err := m.fitsNow(id, 0); err != nil {
			why[id] = err
			continue
		}
		first = i
		break
	}
	ci := slices.Index(list, cur)
	switch {
	case first < 0:
		chosen = list[len(list)-1] // none fits: the last one, with its admission error
		c.streak = 0
	case ci < 0 || first >= ci:
		chosen = list[first] // first choice, or the current one no longer fits
		c.streak = 0
	default:
		// An earlier default fits again: move up only when it fits by a
		// margin at upgradeSamples decision points in a row.
		chosen = cur
		if m.fitsNow(list[first], upgradeMargin) == nil {
			c.streak++
			if c.streak >= upgradeSamples {
				chosen, c.streak = list[first], 0
			}
		} else {
			c.streak = 0
		}
	}
	if chosen == list[0] {
		return chosen, "", "fits"
	}
	pref := list[0]
	err := why[pref]
	if err == nil { // the preferred model fits but the upgrade is not settled yet
		err = m.fitsNow(pref, upgradeMargin)
	}
	return chosen, m.describeFallback(chosen, pref, err), errCause(err)
}

func errCause(err error) string {
	var ae *memory.AdmissionError
	switch {
	case err == nil:
		return "settling"
	case errors.As(err, &ae):
		return "insufficient_memory"
	case errors.Is(err, errNotInstalled):
		return "not_installed"
	}
	return err.Error()
}

var errNotInstalled = errors.New("not installed")

func (m *LLM) modelName(id string) string {
	if s := m.d.Catalog.ByID(id); s != nil {
		return s.Name()
	}
	return id
}

// describeFallback is the short, factual reason the UI shows.
func (m *LLM) describeFallback(chosen, preferred string, err error) string {
	why := "needs more free memory than is available"
	var ae *memory.AdmissionError
	switch {
	case err == nil:
		why = "fits again; LocalBase switches back once it has room to spare"
	case errors.Is(err, errNotInstalled):
		why = "is not installed"
	case !errors.As(err, &ae):
		why = "cannot start: " + strings.TrimSuffix(err.Error(), ".")
	}
	return fmt.Sprintf("Using %s: %s %s.", m.modelName(chosen), m.modelName(preferred), why)
}

// busy: the LLM is serving, starting or has work queued.
func (m *LLM) busy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active > 0 || m.starting || len(m.queue) > 0
}

// fitsNow is the admission check a switch to id would face now, plus
// margin bytes, without starting or stopping anything.
func (m *LLM) fitsNow(id string, margin int64) error {
	m.mu.Lock()
	running := m.cur != nil && m.cur.modelID == id
	m.mu.Unlock()
	if running && margin == 0 {
		return nil
	}
	spec := m.d.Catalog.ByID(id)
	t, err := m.target(id)
	if err != nil {
		return err
	}
	if spec != nil && (t == nil || !t.Imported) {
		if complete, _ := spec.Installation(m.d.ModelsDir); !complete && !running {
			return errNotInstalled
		}
	}
	plan, err := m.planFor(id, t)
	if err != nil {
		return err
	}
	if t != nil && t.Imported {
		spec = nil
	}
	demand := m.admissionDemand(spec, plan)
	if t != nil && t.Backend.Adapter == "process-openai/v1" {
		demand = customDemand(t, plan)
	}
	if margin > 0 {
		pad := max(margin, demand.UnifiedBytes/10)
		demand.UnifiedBytes += pad
		demand.HostBytes += pad
		demand.AcceleratorBytes += pad
	}
	return m.d.Memory.CheckAdmission(context.Background(), demand, append([]string{"llm"}, m.d.Recovery.idleIDs("llm")...))
}

// isChat: a chat-capable LLM (not a decision or embedding model). Models
// outside the catalog (imported) are taken as chat.
func (m *LLM) isChat(id string) bool {
	s := m.d.Catalog.ByID(id)
	return s == nil || (s.Kind == "llm" && !s.IsDecision() && !s.IsEmbedding())
}

// chatDefault is the single-entry default: the active LLM when it is a chat
// model, else the first selected chat model. It is never a decision or
// embedding model, whatever ran last (operator, after cutover).
func (m *LLM) chatDefault(active string, selected []string) string {
	if active != "" && m.isChat(active) {
		return active
	}
	for _, id := range selected {
		if m.isChat(id) {
			return id
		}
	}
	return active
}
