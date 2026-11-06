package runtime

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

// The fit-aware default chat model (DESIGN §18): modelA (9B) is preferred,
// modelB (4B) the fallback.

type defaultFixture struct {
	m    *LLM
	host *stepHost
	now  time.Time
	logs *syncBuffer
	// tight is available memory where B fits and A does not; roomy fits A
	// by more than the upgrade margin.
	tight, roomy int64
}

func newDefaultFixture(t *testing.T, defaults []string) *defaultFixture {
	t.Helper()
	host := newStepHost(50 * memory.GiB)
	mem := memory.NewController(host, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	mem.SetSampler(nil)
	m := newTestLLMWith(t, mem)
	f := &defaultFixture{m: m, host: host, now: time.Unix(1_000_000, 0), logs: &syncBuffer{}}
	m.d.Log = slog.New(slog.NewJSONHandler(f.logs, nil))
	m.chooser.now = func() time.Time { return f.now }
	cfg := m.Config().Clone()
	cfg.DefaultLLMs = defaults
	m.SetConfig(cfg)
	// Find the memory levels from the admission check itself.
	for g := int64(4); g <= 64*4; g++ {
		host.avail.Store(g * memory.GiB / 4)
		a, b := m.fitsNow(modelA, 0), m.fitsNow(modelB, 0)
		if f.tight == 0 && a != nil && b == nil {
			f.tight = g * memory.GiB / 4
		}
		if a == nil && m.fitsNow(modelA, upgradeMargin) == nil {
			f.roomy = g*memory.GiB/4 + memory.GiB
			break
		}
	}
	if f.tight == 0 || f.roomy == 0 {
		t.Fatalf("no memory level separates the models (tight %d, roomy %d)", f.tight, f.roomy)
	}
	return f
}

// decide advances past the dwell and takes a decision point.
func (f *defaultFixture) decide() DefaultChoice {
	f.now = f.now.Add(defaultDwell + time.Second)
	return f.m.DefaultModel(true)
}

func TestDefaultPreferredFits(t *testing.T) {
	f := newDefaultFixture(t, []string{modelA, modelB})
	f.host.avail.Store(f.roomy)
	if c := f.decide(); c.ModelID != modelA || c.Reason != "" || c.Preferred != modelA {
		t.Fatalf("choice %+v", c)
	}
	if id, _, err := f.m.Resolve(""); err != nil || id != modelA {
		t.Errorf("an unnamed request resolves to %q (%v)", id, err)
	}
}

func TestDefaultFallsBackWithReason(t *testing.T) {
	f := newDefaultFixture(t, []string{modelA, modelB})
	f.host.avail.Store(f.tight)
	c := f.decide()
	if c.ModelID != modelB {
		t.Fatalf("choice %+v", c)
	}
	if want := "Using Qwen3.5 4B: Qwen3.5 9B needs more free memory than is available."; c.Reason != want {
		t.Errorf("reason %q, want %q", c.Reason, want)
	}
	if id, _, _ := f.m.Resolve(""); id != modelB {
		t.Errorf("an unnamed request resolves to %q", id)
	}
	if log := f.logs.String(); !strings.Contains(log, `"event":"model.default-selected"`) ||
		!strings.Contains(log, `"preferred_model_id":"`+modelA+`"`) || !strings.Contains(log, `"reason":"insufficient_memory"`) {
		t.Errorf("event not logged: %s", log)
	}
}

func TestDefaultMovesBackAfterHysteresis(t *testing.T) {
	f := newDefaultFixture(t, []string{modelA, modelB})
	f.host.avail.Store(f.tight)
	if c := f.decide(); c.ModelID != modelB {
		t.Fatalf("choice %+v", c)
	}
	f.host.avail.Store(f.roomy)
	// Within the dwell nothing is re-evaluated.
	if c := f.m.DefaultModel(true); c.ModelID != modelB {
		t.Fatalf("re-evaluated within the dwell: %+v", c)
	}
	for i := 1; i < upgradeSamples; i++ {
		if c := f.decide(); c.ModelID != modelB || !strings.Contains(c.Reason, "fits again") {
			t.Fatalf("decision %d moved early: %+v", i, c)
		}
	}
	if c := f.decide(); c.ModelID != modelA || c.Reason != "" {
		t.Fatalf("not back to the preferred model: %+v", c)
	}
	// A sample that fits without the margin resets the count.
	f.host.avail.Store(f.tight)
	f.decide()
	f.host.avail.Store(f.roomy)
	f.decide()
	f.host.avail.Store(f.tight + (f.roomy-f.tight)/100) // A fits, not by the margin
	if f.m.fitsNow(modelA, 0) == nil && f.m.fitsNow(modelA, upgradeMargin) != nil {
		f.decide()
		f.host.avail.Store(f.roomy)
		f.decide()
		if c := f.decide(); c.ModelID != modelB {
			t.Errorf("moved up without %d margin samples in a row: %+v", upgradeSamples, c)
		}
	}
}

func TestDefaultNoneFitsUsesLast(t *testing.T) {
	f := newDefaultFixture(t, []string{modelA, modelB})
	f.host.avail.Store(memory.GiB)
	if c := f.decide(); c.ModelID != modelB {
		t.Fatalf("choice %+v", c)
	}
	// The last default then gets the normal admission error.
	_, err := f.m.Acquire(context.Background(), modelB)
	var ae *memory.AdmissionError
	if !errors.As(err, &ae) {
		t.Fatalf("acquire: %v", err)
	}
}

func TestExplicitRequestIsNeverSubstituted(t *testing.T) {
	f := newDefaultFixture(t, []string{modelA, modelB})
	f.host.avail.Store(f.tight)
	f.decide()
	id, _, err := f.m.Resolve(modelA)
	if err != nil || id != modelA {
		t.Fatalf("resolve %q: %v", id, err)
	}
	_, err = f.m.Acquire(context.Background(), modelA)
	var ae *memory.AdmissionError
	if !errors.As(err, &ae) || ae.Permanent {
		t.Fatalf("explicit request: %v, want a retryable admission error", err)
	}
}

func TestDefaultDoesNotChangeMidStream(t *testing.T) {
	f := newDefaultFixture(t, []string{modelA, modelB})
	f.host.avail.Store(f.roomy)
	if c := f.decide(); c.ModelID != modelA {
		t.Fatalf("choice %+v", c)
	}
	l, err := f.m.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	f.host.avail.Store(memory.GiB) // A could not start now, but it is streaming
	if c := f.decide(); c.ModelID != modelA {
		t.Errorf("changed while a request was active: %+v", c)
	}
	l.Release()
	// The resident, idle model is credited: it still fits where it runs.
	f.host.avail.Store(f.tight)
	if c := f.decide(); c.ModelID != modelA {
		t.Errorf("the running preferred model was dropped: %+v", c)
	}
}

func TestSingleDefaultIsTheActiveModel(t *testing.T) {
	f := newDefaultFixture(t, nil)
	f.host.avail.Store(f.tight) // even when it does not fit
	if c := f.decide(); c.ModelID != modelA || c.Reason != "" {
		t.Fatalf("choice %+v", c)
	}
	if id, _, _ := f.m.Resolve(""); id != modelA {
		t.Errorf("resolve %q", id)
	}
	if strings.Contains(f.logs.String(), "model.default-selected") {
		t.Error("a single default logged a selection")
	}
}

// A decision or embedding request never becomes the default chat model
// (operator, after cutover: one decision request made clef-flash the
// active LLM, and status reported it as the default chat model).
func TestDecisionRequestDoesNotBecomeDefault(t *testing.T) {
	const decision = "clef-flash-q4_k_m"
	host := newStepHost(50 * memory.GiB)
	mem := memory.NewController(host, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	mem.SetSampler(nil)
	m := newTestLLMWith(t, mem)
	installSparse(t, m.d.ModelsDir, m.d.Catalog.ByID(decision))
	cfg := m.Config().Clone()
	cfg.Selected["llm"] = append(cfg.Selected["llm"], decision)
	m.SetConfig(cfg)
	var activated []string
	m.d.Activate = func(kind, id string) {
		activated = append(activated, id)
		next := m.Config().Clone()
		next.Active[kind] = id
		m.SetConfig(next)
	}
	for _, id := range []string{modelB, decision} {
		l, err := m.Acquire(context.Background(), id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		l.Release()
	}
	if len(activated) != 1 || activated[0] != modelB {
		t.Errorf("activated %v, want only %s", activated, modelB)
	}
	if c := m.DefaultModel(true); c.ModelID != modelB {
		t.Errorf("default %+v after a decision request, want %s", c, modelB)
	}
	// A decision model already saved as the active LLM (as on the live
	// root before this fix) is never the default: the first selected chat
	// model is.
	cfg = m.Config().Clone()
	cfg.Active["llm"] = decision
	m.SetConfig(cfg)
	if c := m.DefaultModel(true); c.ModelID != modelA {
		t.Errorf("default %+v with a decision model active, want %s", c, modelA)
	}
}
