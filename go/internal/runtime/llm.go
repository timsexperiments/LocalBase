package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/otlp"
	"github.com/timsexperiments/LocalBase/go/internal/placement"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

var (
	ErrQueueFull     = errors.New("inference queue is full")
	ErrQueueTimeout  = errors.New("inference request exceeded its queue wait deadline")
	ErrUnavailable   = errors.New("runtime unavailable")
	ErrModelNotFound = errors.New("model not found")
	ErrStartupFailed = errors.New("runtime failed to start")
	// ErrMemoryEmergency rejects queued work when memory turns critical (TS:
	// "Inference rejected by memory emergency.", a 503 for "Inference").
	ErrMemoryEmergency = fmt.Errorf("%w: inference rejected by memory emergency", ErrUnavailable)
)

const (
	defaultStartupBudget = 60 * time.Second
	largeStartupBudget   = 180 * time.Second
	largeStartupWeights  = 10 * memory.GiB
	stopGrace            = 10 * time.Second
	// loadStopGrace is how long a backend still loading gets after SIGTERM: it serves nobody yet
	// and holds nothing worth saving, and one that won't stop must not use up the service
	// manager's limit on a gateway that is stopping.
	loadStopGrace = 2 * time.Second
)

// LLMDeps are the LLM manager's collaborators.
type LLMDeps struct {
	Root        string
	ModelsDir   string
	RuntimesDir string
	Store       *store.Store
	Catalog     *catalog.Catalog
	Memory      *memory.Controller
	Log         *slog.Logger
	MemoryGB    float64
	CtxOverride int
	// ModelFileOverride replaces the active model's file (serve --llm-model-file).
	ModelFileOverride string
	PortOverride      int
	QueueCapacity     int
	QueueTimeout      time.Duration
	// Binary overrides managed resolution (tests: a fake backend).
	Binary string
	// BeforeSwitch runs before a switch computes admission (serve: read the GPU
	// limit, if a runtime installed since startup makes that possible).
	BeforeSwitch func()
	// Install downloads a missing model (serve --install-missing); nil means
	// requests for missing models fail with NotInstalledError.
	Install func(ctx context.Context, modelID string) error
	// Gate resolves the backend selection (nil: every model runs on the
	// managed llama-server, as before backend selection).
	Gate Gate
	// Activate persists modelID as the active model of kind after a switch
	// to it (TS activateModel); nil skips it.
	Activate func(kind, modelID string)
	// SelectionKey identifies a model's current backend selection cheaply;
	// a running model whose key changed is restarted on its next request.
	SelectionKey func(modelID string) string
	// Otel traces backend starts (nil: disabled).
	Otel *otlp.Runtime
	// Recovery stops idle runtimes of other modalities when a model is
	// refused for memory (nil: no recovery).
	Recovery *IdleRecovery
	// Placement supplies a model's stored CPU/GPU placement and the device
	// it would run on (DESIGN §20; nil: no placement planning).
	Placement PlacementSource
	// GPUPool admits a discrete placement's GPU bytes on gpu:0 (DESIGN
	// §20.4; nil: no GPU pool).
	GPUPool *memory.GPUPool
}

// PlacementSource is where a load's placement inputs come from.
type PlacementSource interface {
	// Request is the stored choice for a model on this device.
	Request(modelID string) placement.Request
	// Device describes this machine and the target backend now.
	Device(t *Target) placement.Device
}

type backend struct {
	modelID string
	key     string
	spec    *catalog.Spec
	target  *Target
	demand  memory.Demand
	plan    *LLMPlan
	proc    *Process
	res     *memory.Reservation
	baseURL string
	stop    chan struct{}
	// stopOnce covers the whole end of the backend, once whoever gets there
	// first: a switch and an eviction can capture the same backend, and a
	// caller that comes second waits here until the memory is released.
	stopOnce sync.Once
	// sampling ends with the backend: a stop does not wait out a memory sample.
	sampling     context.Context
	stopSampling context.CancelFunc
	// emergency: stopped because memory turned critical.
	emergency atomic.Bool
}

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

// LLM manages the one llama-server the gateway runs: a FIFO queue in front
// of it, model switches, and memory admission. It replaces the TS
// reconciler, supervisor registry and factory for this modality.
type LLM struct {
	d LLMDeps

	mu       sync.Mutex
	cfg      *store.Config
	cur      *backend
	starting bool
	// cancelSwitch ends the switch in flight, install and start included, with a
	// cause its waiters get. An eviction or a shutdown calls it, with mu held; a
	// start that was ready just before finds out in install, also with mu held.
	cancelSwitch context.CancelCauseFunc
	stopping     bool // the gateway is stopping: no model starts, and what is loaded still serves
	active       int
	queue        []*waiter
	closed       bool
	geometry     map[string]geometryEntry
	probes       sync.WaitGroup
	watches      sync.WaitGroup // backend watchers; each can end in a store write
	switches     sync.WaitGroup // in-flight model switches; Close waits for them
	failed       bool           // the last start failed and nothing runs
	claimed      bool           // idle recovery is stopping the backend; hold requests
	unloading    string         // the model draining for an explicit unload (DESIGN §23)
	unloaders    int            // the unloads draining it
	chooser      defaultChooser // the fit-aware default chat model
}

type geometryEntry struct {
	mod time.Time
	g   *memory.Geometry
}

func NewLLM(d LLMDeps, cfg *store.Config) *LLM {
	if d.QueueCapacity <= 0 {
		d.QueueCapacity = 16
	}
	if d.QueueTimeout <= 0 {
		d.QueueTimeout = 60 * time.Second
	}
	return &LLM{d: d, cfg: cfg, geometry: map[string]geometryEntry{}}
}

// Config returns the configuration snapshot the manager runs with.
func (m *LLM) Config() *store.Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// ConfiguredIDs is the active model plus the selected ones, in order.
func ConfiguredIDs(cfg *store.Config, kind string) []string {
	var ids []string
	for _, id := range append([]string{cfg.Active[kind]}, cfg.Selected[kind]...) {
		if id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// Resolve maps a requested model ("" = the active one) to a configured id.
func (m *LLM) Resolve(model string) (string, *catalog.Spec, error) {
	cfg := m.Config()
	if model == "" {
		model = m.DefaultModel(true).ModelID
	}
	if !slices.Contains(ConfiguredIDs(cfg, "llm"), model) {
		// Imported models run without being configured.
		if t, _ := m.target(model); t == nil || !t.Imported || t.task() == "" {
			return "", nil, ErrModelNotFound
		}
	}
	return model, m.d.Catalog.ByID(model), nil
}

func (m *LLM) port() int {
	if m.d.PortOverride > 0 {
		return m.d.PortOverride
	}
	return m.Config().Port
}

// geometryFor caches GGUF geometry by path and modification time.
func (m *LLM) geometryFor(path string) *memory.Geometry {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	m.mu.Lock()
	e, ok := m.geometry[path]
	m.mu.Unlock()
	if ok && e.mod.Equal(info.ModTime()) {
		return e.g
	}
	g := memory.ReadGeometry(path)
	m.mu.Lock()
	m.geometry[path] = geometryEntry{info.ModTime(), g}
	m.mu.Unlock()
	return g
}

// target resolves the model through the gate; nil without one. A target may
// come back together with a *LaunchError.
func (m *LLM) target(modelID string) (*Target, error) {
	if m.d.Gate == nil {
		return nil, nil
	}
	return m.d.Gate(modelID)
}

// Plan computes the launch plan for a model without starting anything.
func (m *LLM) Plan(modelID string) (*LLMPlan, error) {
	t, _ := m.target(modelID)
	return m.planFor(modelID, t)
}

func (m *LLM) planFor(modelID string, t *Target) (*LLMPlan, error) {
	if t != nil && t.Imported {
		if t.Variant == nil {
			if t.Model == nil || len(t.Model.Variants) == 0 {
				return nil, errors.New("the model has no variant")
			}
			t.Variant = &t.Model.Variants[0] // listing a model without a selection
		}
		w, err := t.weights()
		if err != nil {
			return nil, err
		}
		in := PlanInput{CPUOnly: m.cpuOnly(t), Config: m.Config(), ModelID: modelID, ModelsDir: t.Dir, ModelFile: w, MemoryGB: m.d.MemoryGB,
			CtxOverride: m.d.CtxOverride, Port: m.port(), Task: t.task(), Geometry: m.geometryFor(filepath.Join(t.Dir, w))}
		if p := t.projector(); p != "" {
			in.Projector = filepath.Join(t.Dir, p)
		}
		for _, a := range t.Variant.Artifacts {
			if a.Role == "weights" {
				in.ArtifactBytes += fileSize(filepath.Join(t.Dir, a.Filename))
			}
		}
		return PlanLLM(in)
	}
	spec := m.d.Catalog.ByID(modelID)
	file := modelID + ".gguf"
	if spec != nil {
		file = spec.Primary().Filename
	}
	override := ""
	if m.d.ModelFileOverride != "" && modelID == m.Config().Active["llm"] {
		override, file = m.d.ModelFileOverride, m.d.ModelFileOverride
	}
	return PlanLLM(PlanInput{CPUOnly: m.cpuOnly(t), Config: m.Config(), Spec: spec, ModelID: modelID, ModelsDir: m.d.ModelsDir,
		MemoryGB: m.d.MemoryGB, CtxOverride: m.d.CtxOverride, Port: m.port(),
		ModelFile: override,
		Geometry:  m.geometryFor(filepath.Join(m.d.ModelsDir, file))})
}

// Running reports the resident model and its plan, if any.
func (m *LLM) Running() (string, *LLMPlan, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil {
		return "", nil, m.active
	}
	return m.cur.modelID, m.cur.plan, m.active
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

// switchTo replaces the running model. Preflight checks admission as if the
// current model were stopped, crediting it at most its measured footprint
// (#355), so a switch that cannot fit never stops the current model. If the
// new model fails after the old one stopped, the old one is restored (#296).
func (m *LLM) switchTo(ctx context.Context, modelID, key string, parent *otlp.Span) {
	err := m.d.Recovery.retryAfterRecovery("llm", func() (memory.Demand, error) {
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
	t, err := m.target(modelID)
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
			if err := settleAfterStop(m.d.Memory, demand); err != nil {
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

// SetConfig replaces the configuration snapshot (after the active model
// changed).
func (m *LLM) SetConfig(cfg *store.Config) {
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
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

// specFor is a model's catalog entry, unless it runs as an import.
func (m *LLM) specFor(modelID string, t *Target) *catalog.Spec {
	if t != nil && t.Imported {
		return nil
	}
	return m.d.Catalog.ByID(modelID)
}

// demandFor is the demand this launch is admitted with.
func (m *LLM) demandFor(modelID string, plan *LLMPlan, t *Target) memory.Demand {
	if t != nil && t.Backend.Adapter == "process-openai/v1" {
		return customDemand(t, plan)
	}
	return m.admissionDemand(m.specFor(modelID, t), plan)
}

// admissionDemand applies observed > measured > estimate (feed: milestone 5).
// Evidence applies only when the launch loads the catalog's primary file.
// admissionDemand is the demand a launch is admitted with, most trusted
// first (TS admissionDemand): this machine's observed peak, the checked-in
// measurement, a feed peak for exactly this file and launch shape, the
// estimate.
func (m *LLM) admissionDemand(spec *catalog.Spec, plan *LLMPlan) memory.Demand {
	// Evidence only applies when the launch loads the catalog primary file.
	if spec == nil || filepath.Base(plan.ModelPath) != spec.Primary().Filename {
		return plan.Demand
	}
	shape := plan.Launch.LaunchShape()
	if sha := spec.Primary().SHA256; sha != "" {
		if peaks, err := m.d.Store.ObservedPeaks(context.Background(), spec.ModelID, sha); err == nil {
			if d := ApplyObserved(plan.Demand, peaks, shape); d.Confidence == "observed" {
				return d
			}
		}
	}
	if plan.Demand.Confidence == "measured" || m.d.Catalog == nil {
		return plan.Demand
	}
	var best *catalog.MemoryPeak
	for _, p := range m.d.Catalog.PlannableFeedPeaks(spec) {
		if p.LaunchShape == shape && (best == nil || p.PeakBytes > best.PeakBytes) {
			p := p
			best = &p
		}
	}
	if best == nil {
		return plan.Demand
	}
	catalogBytes := spec.TotalArtifactBytes()
	feed := memory.FeedDemand(spec, *best, catalogBytes, plan.Launch.PromptCacheBytes)
	// The feed measured the catalog file; a larger local file adds its extra.
	extra := max(plan.ArtifactBytes, catalogBytes) - catalogBytes
	feed.UnifiedBytes += extra
	feed.HostBytes += extra
	feed.AcceleratorBytes += extra
	return feed
}

func (m *LLM) startBackend(life context.Context, modelID string, spec *catalog.Spec, plan *LLMPlan, demand memory.Demand, t *Target, parent *otlp.Span) (*backend, error) {
	ctx := otlp.WithSpan(context.Background(), parent)
	fail := func(err error) error {
		logStartFailed(m.d.Log, "llm", err)
		if t != nil && (t.Manual || t.Imported) {
			return &BackendStartError{BackendID: t.Backend.ID, Cause: err}
		}
		return fmt.Errorf("%w: %v", ErrStartupFailed, err)
	}
	if !fileExists(plan.ModelPath) {
		return nil, fail(fmt.Errorf("model file %s is not installed", filepath.Base(plan.ModelPath)))
	}
	name, bin, args, health, err := m.launchCommand(life, plan, t)
	if err != nil {
		return nil, fail(err)
	}
	if err := checkPortFree(plan.Host, plan.Port); err != nil {
		return nil, fail(err)
	}
	res, err := m.d.Memory.Reserve(context.Background(), "llm", demand)
	if err != nil {
		logAdmissionRejected(m.d.Log, "llm", name, err)
		return nil, err
	}
	// A discrete placement also reserves its GPU bytes on gpu:0.
	var gpuRes *memory.GPUReservation
	if p := plan.Placement; p != nil && p.Platform == placement.Discrete && p.GPULayers > 0 && m.d.GPUPool != nil {
		if gpuRes, err = m.d.GPUPool.Admit("llm", p.GPUBytes); err != nil {
			res.Release()
			logAdmissionRejected(m.d.Log, "llm", name, err, "model_id", modelID, "placement_mode", p.Mode)
			return nil, err
		}
	}
	defer gpuRes.Release() // resident (or failed) by the time this returns
	m.d.Log.InfoContext(ctx, "Starting "+name+" for "+modelID+".", "event", "runtime.starting", "runtime", "llm",
		"model_id", modelID, "ctx_size", plan.CtxSize, "slots", plan.Parallel.Slots,
		"demand_bytes", demand.UnifiedBytes, "demand_confidence", demand.Confidence)
	var proc *Process
	err = spanned(m.d.Otel, ctx, "localbase.backend.start", name, func() (err error) {
		proc, err = StartProcess(name, bin, args, nil, m.d.Log)
		return err
	})
	if err != nil {
		res.Release()
		return nil, fail(err)
	}
	recordBackend(m.d.Root, "llm", plan.Port, proc, m.d.Log)
	proc.ForeignListenerOK = t != nil && t.Backend != nil && t.Backend.Launch != nil
	budget := defaultStartupBudget
	if plan.ArtifactBytes >= largeStartupWeights {
		budget = largeStartupBudget
	}
	base := "http://" + net.JoinHostPort(plan.Host, strconv.Itoa(plan.Port))
	if err := spanned(m.d.Otel, ctx, "localbase.backend.model_load", name, func() error {
		return proc.WaitHealthy(life, base+health, budget)
	}); err != nil {
		proc.Stop(loadStopGrace)
		res.Release()
		return nil, fail(err)
	}
	if err := m.checkPlacement(proc, plan); err != nil {
		proc.Stop(stopGrace)
		res.Release()
		logStartFailed(m.d.Log, "llm", err)
		return nil, err
	}
	res.Materialize(proc.PID())
	m.d.Log.InfoContext(ctx, name+" is ready for "+modelID+".", "event", "runtime.ready", "runtime", "llm", "model_id", modelID, "pid", proc.PID())
	sampling, stopSampling := context.WithCancel(context.Background())
	return &backend{modelID: modelID, spec: spec, target: t, demand: demand, plan: plan, proc: proc, res: res, baseURL: base,
		stop: make(chan struct{}), sampling: sampling, stopSampling: stopSampling}, nil
}

func (m *LLM) stopBackend(b *backend, reason string) {
	m.mu.Lock()
	if m.cur == b {
		m.cur = nil
	}
	m.mu.Unlock()
	b.stopOnce.Do(func() {
		b.stopSampling()
		close(b.stop)
		b.proc.Stop(stopGrace)
		logStopped(m.d.Log, "llm", b.modelID, reason, b.res.Bytes(), b.proc.PID())
		b.res.Release()
	})
}

// watch samples the observed peak and handles an unexpected exit: the slot
// is cleared and the next request starts the model again.
// samplePeriod is how often a running model's memory is sampled. A variable so a test can shorten it.
var samplePeriod = 5 * time.Second

func (m *LLM) watch(b *backend) {
	defer b.stopSampling()
	tick := time.NewTicker(samplePeriod)
	defer tick.Stop()
	var peak int64
	record := b.spec != nil && b.spec.Primary().SHA256 != "" && goruntime.GOOS == "darwin"
	sample := func() {
		if !record {
			return
		}
		ctx, cancel := context.WithTimeout(b.sampling, 4*time.Second)
		bytes, ok := sampleFootprint(ctx, b.proc.PID())
		cancel()
		// Stopped meanwhile: leave the store alone. A write already begun finishes.
		if ok && bytes > peak && b.sampling.Err() == nil {
			// Only a recorded peak counts: a failed write comes round again at the next sample.
			if err := m.d.Store.RecordObservedPeak(context.Background(), store.ObservedPeak{ModelID: b.modelID,
				ArtifactSHA256: b.spec.Primary().SHA256, LaunchShape: b.plan.Launch.LaunchShape(),
				PeakBytes: bytes, Method: "footprint"}); err != nil {
				m.d.Store.WriteFailed("observed-peak", err)
			} else {
				peak = bytes
			}
		}
	}
	sample()
	for {
		select {
		case <-b.stop:
			return
		case <-b.proc.Done():
			m.mu.Lock()
			crashed := m.cur == b
			if crashed {
				m.cur = nil
			}
			m.mu.Unlock()
			if crashed {
				m.d.Log.Error("llama-server exited unexpectedly.", "event", "runtime.exited", "runtime", "llm",
					"model_id", b.modelID, "error", fmt.Sprint(b.proc.ExitErr()))
				b.stopOnce.Do(func() {
					logStopped(m.d.Log, "llm", b.modelID, StopCrash, b.res.Bytes(), b.proc.PID())
					b.res.Release()
				})
				m.mu.Lock()
				m.pump()
				m.mu.Unlock()
			}
			return
		case <-tick.C:
			sample()
		}
	}
}

// EmergencyStop stops the runtime and rejects queued work (memory critical).
func (m *LLM) EmergencyStop(reason error) {
	m.mu.Lock()
	b := m.cur
	if b != nil {
		b.emergency.Store(true)
	}
	for len(m.queue) > 0 {
		m.fail(m.queue[0], reason)
	}
	if m.cancelSwitch != nil {
		m.cancelSwitch(reason)
	}
	m.mu.Unlock()
	if b != nil {
		m.stopBackend(b, StopMemory)
	}
	m.probes.Wait()
}

// Close rejects queued work and stops the runtime.
// StopLoads ends a load in flight and starts no other. The gateway calls it first when it stops:
// waiting for requests to drain would otherwise wait on a load nobody will use. A model already
// loaded goes on serving what it holds until Close.
func (m *LLM) StopLoads() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopping = true
	if m.cancelSwitch != nil {
		m.cancelSwitch(ErrUnavailable)
	}
	m.pump()
}

func (m *LLM) Close() {
	m.mu.Lock()
	m.closed = true
	for len(m.queue) > 0 {
		m.fail(m.queue[0], ErrUnavailable)
	}
	if m.cancelSwitch != nil {
		m.cancelSwitch(ErrUnavailable)
	}
	m.mu.Unlock()
	// A switch in flight finishes (and sees closed) before the backend stops,
	// and probes finish before the store they write to is closed.
	m.switches.Wait()
	m.mu.Lock()
	b := m.cur
	m.mu.Unlock()
	if b != nil {
		m.stopBackend(b, StopShutdown)
	}
	m.watches.Wait()
	m.probes.Wait()
}

// Footprint samples the running backend's memory footprint (macOS).
func (m *LLM) Footprint(ctx context.Context) (int64, bool) {
	m.mu.Lock()
	b := m.cur
	m.mu.Unlock()
	if b == nil {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return memory.Footprint(ctx, b.proc.PID())
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

// Snapshot is the runtime's lifecycle state for health and metadata.
type Snapshot struct {
	State     string // disabled | idle | starting | running | draining | stopping | failed
	ModelID   string // running model, else the active one
	Slots     int    // execution slots of the running model, 0 if none
	Active    int
	Waiting   int
	Capacity  int
	Accepting bool
}

// LoadedOrLoading returns the models this manager holds: the running one and
// any being started. Unlike Snapshot it never reports the configured default,
// so an idle manager holds nothing.
func (m *LLM) LoadedOrLoading() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	if m.cur != nil {
		ids = append(ids, m.cur.modelID)
	}
	if m.starting {
		for _, w := range m.queue {
			if w.dispatching {
				ids = append(ids, w.model)
			}
		}
	}
	return ids
}

func (m *LLM) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Snapshot{ModelID: m.cfg.Active["llm"], Active: m.active, Waiting: len(m.queue), Capacity: m.d.QueueCapacity,
		Accepting: !m.closed}
	switch {
	case m.closed:
		s.State = "stopping"
	case m.starting:
		s.State = "starting"
	case m.cur != nil:
		s.State, s.ModelID, s.Slots = "running", m.cur.modelID, m.cur.plan.Parallel.Slots
		if len(m.queue) > 0 && m.queue[0].model != m.cur.modelID && m.active > 0 {
			s.State = "draining"
		}
	case m.failed:
		s.State = "failed"
	default:
		s.State = "idle"
	}
	if len(ConfiguredIDs(m.cfg, "llm")) == 0 {
		s.State = "disabled"
	}
	return s
}

// restore starts the previous model again after a switch failed.
func (m *LLM) restore(ctx context.Context, prev *backend, parent *otlp.Span) {
	if rb, err := m.startBackend(ctx, prev.modelID, prev.spec, prev.plan, prev.demand, prev.target, parent); err == nil {
		rb.key = prev.key
		_ = m.install(ctx, rb) // its only refusal is an ended switch, which already reported
	}
}

// Interrupted reports whether the lease's runtime was stopped because memory
// turned critical; the request then answers 503 with Retry-After.
func (l *Lease) Interrupted() bool { return l.b != nil && l.b.emergency.Load() }
