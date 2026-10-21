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
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

var (
	ErrQueueFull     = errors.New("inference queue is full")
	ErrQueueTimeout  = errors.New("inference request exceeded its queue wait deadline")
	ErrUnavailable   = errors.New("runtime unavailable")
	ErrModelNotFound = errors.New("model not found")
	ErrStartupFailed = errors.New("runtime failed to start")
)

const (
	defaultStartupBudget = 60 * time.Second
	largeStartupBudget   = 180 * time.Second
	largeStartupWeights  = 10 * memory.GiB
	stopGrace            = 10 * time.Second
	samplePeriod         = 5 * time.Second
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
}

type waiter struct {
	model       string
	key         string
	ready       chan error
	lease       *Lease
	dispatching bool
	done        bool
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
	active   int
	queue    []*waiter
	closed   bool
	geometry map[string]geometryEntry
	probes   sync.WaitGroup
	switches sync.WaitGroup // in-flight model switches; Close waits for them
	failed   bool           // the last start failed and nothing runs
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
		model = cfg.Active["llm"]
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
	return m.cfg.Port
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
		in := PlanInput{Config: m.Config(), ModelID: modelID, ModelsDir: t.Dir, ModelFile: w, MemoryGB: m.d.MemoryGB,
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
	return PlanLLM(PlanInput{Config: m.Config(), Spec: spec, ModelID: modelID, ModelsDir: m.d.ModelsDir,
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
	if len(m.queue) >= m.d.QueueCapacity {
		m.mu.Unlock()
		return nil, ErrQueueFull
	}
	w := &waiter{model: modelID, key: key, ready: make(chan error, 1)}
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
			w.lease.Waited = time.Since(start)
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
	for len(m.queue) > 0 && !m.starting && !m.closed {
		head := m.queue[0]
		if m.cur != nil && m.cur.modelID == head.model && m.cur.key == head.key {
			if m.active >= m.cur.plan.Parallel.Slots {
				return
			}
			m.grant(head)
			continue
		}
		if m.active > 0 {
			return // drain the current model first
		}
		m.starting = true
		head.dispatching = true
		m.switches.Add(1)
		go func(model, key string) {
			defer m.switches.Done()
			m.switchTo(model, key)
		}(head.model, head.key)
		return
	}
}

// switchTo replaces the running model. Preflight checks admission as if the
// current model were stopped, crediting it at most its measured footprint
// (#355), so a switch that cannot fit never stops the current model. If the
// new model fails after the old one stopped, the old one is restored (#296).
func (m *LLM) switchTo(modelID, key string) {
	err := m.doSwitch(modelID, key)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.starting = false
	m.failed = err != nil && m.cur == nil
	if err != nil {
		for len(m.queue) > 0 && m.queue[0].model == modelID && m.queue[0].key == key {
			m.fail(m.queue[0], err)
		}
	}
	m.pump()
}

func (m *LLM) doSwitch(modelID, key string) error {
	// Catalog models keep the install check (and its consent error) first;
	// then the selection: an unselected or unavailable backend fails before
	// the current model is stopped.
	if m.d.Catalog.ByID(modelID) != nil {
		if err := m.ensureInstalled(modelID); err != nil {
			return err
		}
	}
	t, err := m.target(modelID)
	if err != nil {
		return err
	}
	if t == nil && m.d.Catalog.ByID(modelID) == nil {
		if err := m.ensureInstalled(modelID); err != nil {
			return err
		}
	}
	plan, err := m.planFor(modelID, t)
	if err != nil {
		return err
	}
	spec := m.d.Catalog.ByID(modelID)
	if t != nil && t.Imported {
		spec = nil
	}
	demand := m.admissionDemand(spec, plan)
	if t != nil && t.Backend.Adapter == "process-openai/v1" {
		demand = customDemand(t, plan)
	}
	m.mu.Lock()
	prev := m.cur
	m.mu.Unlock()
	if prev != nil {
		if err := m.d.Memory.CheckAdmission(context.Background(), demand, []string{"llm"}); err != nil {
			m.d.Log.Warn("Switch preflight rejected; keeping the current model.", "event", "runtime.switch-rejected",
				"runtime", "llm", "model_id", modelID, "current_model_id", prev.modelID, "error", err.Error())
			return err
		}
		m.stopBackend(prev)
	}
	b, err := m.startBackend(modelID, spec, plan, demand, t)
	if err != nil {
		// A changed backend for the same model is not rolled back: the
		// person chose it, and the old one must not answer in its place.
		if prev != nil && prev.modelID != modelID {
			m.d.Log.Warn("Switch failed; restoring the previous model.", "event", "runtime.switch-rollback",
				"runtime", "llm", "model_id", modelID, "previous_model_id", prev.modelID, "error", err.Error())
			if rb, rerr := m.startBackend(prev.modelID, prev.spec, prev.plan, prev.demand, prev.target); rerr == nil {
				rb.key = prev.key
				m.install(rb)
			}
		}
		return err
	}
	b.key = key
	m.install(b)
	if m.d.Activate != nil && (t == nil || !t.Imported) {
		m.d.Activate("llm", modelID)
	}
	return nil
}

// SetConfig replaces the configuration snapshot (after the active model
// changed).
func (m *LLM) SetConfig(cfg *store.Config) {
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
}

func (m *LLM) install(b *backend) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.stopBackend(b)
		return
	}
	m.cur = b
	m.mu.Unlock()
	go m.watch(b)
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

func (m *LLM) startBackend(modelID string, spec *catalog.Spec, plan *LLMPlan, demand memory.Demand, t *Target) (*backend, error) {
	fail := func(err error) error {
		if t != nil && (t.Manual || t.Imported) {
			return &BackendStartError{BackendID: t.Backend.ID, Cause: err}
		}
		return fmt.Errorf("%w: %v", ErrStartupFailed, err)
	}
	if !fileExists(plan.ModelPath) {
		return nil, fail(fmt.Errorf("model file %s is not installed", filepath.Base(plan.ModelPath)))
	}
	name, bin, args, health, err := m.launchCommand(plan, t)
	if err != nil {
		return nil, fail(err)
	}
	res, err := m.d.Memory.Reserve(context.Background(), "llm", demand)
	if err != nil {
		return nil, err
	}
	m.d.Log.Info("Starting "+name+" for "+modelID+".", "event", "runtime.starting", "runtime", "llm",
		"model_id", modelID, "ctx_size", plan.CtxSize, "slots", plan.Parallel.Slots,
		"demand_bytes", demand.UnifiedBytes, "demand_confidence", demand.Confidence)
	proc, err := StartProcess(name, bin, args, nil, m.d.Log)
	if err != nil {
		res.Release()
		return nil, fail(err)
	}
	res.Materialize(proc.PID())
	budget := defaultStartupBudget
	if plan.ArtifactBytes >= largeStartupWeights {
		budget = largeStartupBudget
	}
	base := "http://" + net.JoinHostPort(plan.Host, strconv.Itoa(plan.Port))
	if err := proc.WaitHealthy(context.Background(), base+health, budget); err != nil {
		proc.Stop(stopGrace)
		res.Release()
		return nil, fail(err)
	}
	m.d.Log.Info(name+" is ready for "+modelID+".", "event", "runtime.ready", "runtime", "llm", "model_id", modelID, "pid", proc.PID())
	return &backend{modelID: modelID, spec: spec, target: t, demand: demand, plan: plan, proc: proc, res: res, baseURL: base, stop: make(chan struct{})}, nil
}

func (m *LLM) stopBackend(b *backend) {
	m.mu.Lock()
	if m.cur == b {
		m.cur = nil
	}
	m.mu.Unlock()
	close(b.stop)
	b.proc.Stop(stopGrace)
	b.res.Release()
}

// watch samples the observed peak and handles an unexpected exit: the slot
// is cleared and the next request starts the model again.
func (m *LLM) watch(b *backend) {
	tick := time.NewTicker(samplePeriod)
	defer tick.Stop()
	var peak int64
	record := b.spec != nil && b.spec.Primary().SHA256 != "" && goruntime.GOOS == "darwin"
	sample := func() {
		if !record {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		bytes, ok := memory.Footprint(ctx, b.proc.PID())
		cancel()
		if ok && bytes > peak {
			peak = bytes
			_ = m.d.Store.RecordObservedPeak(context.Background(), store.ObservedPeak{ModelID: b.modelID,
				ArtifactSHA256: b.spec.Primary().SHA256, LaunchShape: b.plan.Launch.LaunchShape(),
				PeakBytes: bytes, Method: "footprint"})
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
				b.res.Release()
				m.d.Log.Error("llama-server exited unexpectedly.", "event", "runtime.exited", "runtime", "llm",
					"model_id", b.modelID, "error", fmt.Sprint(b.proc.ExitErr()))
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
	for len(m.queue) > 0 {
		m.fail(m.queue[0], reason)
	}
	m.mu.Unlock()
	if b != nil {
		m.stopBackend(b)
	}
	m.probes.Wait()
}

// Close rejects queued work and stops the runtime.
func (m *LLM) Close() {
	m.mu.Lock()
	m.closed = true
	for len(m.queue) > 0 {
		m.fail(m.queue[0], ErrUnavailable)
	}
	m.mu.Unlock()
	// A switch in flight finishes (and sees closed) before the backend stops,
	// and probes finish before the store they write to is closed.
	m.switches.Wait()
	m.mu.Lock()
	b := m.cur
	m.mu.Unlock()
	if b != nil {
		m.stopBackend(b)
	}
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
func (m *LLM) ensureInstalled(modelID string) error {
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
	return m.d.Install(context.Background(), modelID)
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
