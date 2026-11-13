package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/gguf"
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
	// hooks (tests) shorten the sampling and the settle, or hold a sample; the zero value is how
	// the package runs.
	hooks hooks
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
	queue        waitQueue[*Lease]
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
	g   *gguf.Geometry
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
		if t, _ := m.target(context.Background(), model, false); t == nil || !t.Imported || t.task() == "" {
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
func (m *LLM) geometryFor(path string) *gguf.Geometry {
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
	g := gguf.ReadGeometry(path)
	m.mu.Lock()
	m.geometry[path] = geometryEntry{info.ModTime(), g}
	m.mu.Unlock()
	return g
}

// target resolves the model through the gate; nil without one. A target may
// come back together with a *LaunchError. A launch passes its own context, so
// a download the gate starts for it ends with it, and install, which nothing
// else passes: a question about a model downloads nothing.
func (m *LLM) target(ctx context.Context, modelID string, install bool) (*Target, error) {
	if m.d.Gate == nil {
		return nil, nil
	}
	return m.d.Gate(ctx, modelID, install)
}

// Plan computes the launch plan for a model without starting anything.
func (m *LLM) Plan(modelID string) (*LLMPlan, error) {
	t, _ := m.target(context.Background(), modelID, false)
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

// SetConfig replaces the configuration snapshot (after the active model
// changed).
func (m *LLM) SetConfig(cfg *store.Config) {
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
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
