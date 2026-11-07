package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/otlp"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Service supervises the one resident backend of a non-LLM modality (STT,
// TTS, image): a FIFO queue, memory admission before start, model switches,
// and crash recovery on the next request. Requests run one at a time, as in
// TS. The backend comes from the device's selection (M3.5); the adapter
// turns it into a command line.
type Service struct {
	d ServiceDeps

	mu        sync.Mutex
	cur       *svcBackend
	busy      bool
	queue     waitQueue[*ServiceLease]
	closed    bool
	failed    bool
	switching bool   // a model is being stopped or started
	claimed   bool   // idle recovery is stopping the backend; hold requests
	unloading string // the model draining for an explicit unload (DESIGN §23)
	unloaders int    // the unloads draining it
	switchW   sync.WaitGroup
	// cancelSwitch ends the switch in flight, as on the LLM.
	cancelSwitch context.CancelCauseFunc
	// stopping: the gateway is stopping, so no model starts, as on the LLM.
	stopping bool
	watches  sync.WaitGroup // backend watchers; each can end in a store write
	// reloads are rolled-back models reloading in the background; Close cancels them through
	// life and waits for them, so none runs after it.
	reloads sync.WaitGroup
	life    context.Context
	endLife context.CancelFunc
}

// ServiceDeps are a Service's collaborators.
type ServiceDeps struct {
	Modality     string // stt | tts | image
	Kind         string // catalog kind of the modality's models
	Catalog      *catalog.Catalog
	Store        *store.Store
	Memory       *memory.Controller
	Log          *slog.Logger
	Config       func() *store.Config
	Gate         Gate
	SelectionKey func(modelID string) string
	RuntimesDir  string
	ModelsDir    string // catalog models of this modality
	RootDir      string // for custom voices (<root>/voices/<id>.wav)
	// ExtraKey adds to the selection key (TTS: the custom voice set, which
	// the warm speech servers load at launch).
	ExtraKey func() string
	CacheDir string // per-root cache for backends (CrispASR voices, Metal caches)
	Host     string
	Port     int
	// Verify checks a catalog artifact before a video launch (nil:
	// install.Verify, size and sha256 with the checksum cache).
	Verify func(path string, a catalog.Artifact, dir string) error
	// Binaries override managed executables by name (tests: a fake server).
	Binaries      map[string]string
	QueueCapacity int
	QueueTimeout  time.Duration
	Install       func(ctx context.Context, modelID string) error
	Startup       time.Duration
	// Activate persists the active model after a switch (TS activateModel).
	Activate func(kind, modelID string)
	// Otel traces backend starts (nil: disabled).
	Otel *otlp.Runtime
	// Recovery stops idle runtimes of other modalities when a model is
	// refused for memory (nil: no recovery).
	Recovery *IdleRecovery
}

type svcBackend struct {
	modelID string
	key     string
	target  *Target
	proc    *Process
	res     *memory.Reservation
	baseURL string
	// sampling ends with the backend: a stop does not wait out a memory sample.
	sampling     context.Context
	stopSampling context.CancelFunc
	// emergency: stopped because memory turned critical.
	emergency atomic.Bool
}

// svcWaiter is a request in a service's queue.
type svcWaiter = queued[*ServiceLease]

// ServiceLease is exclusive use of the resident backend for one request.
type ServiceLease struct {
	s       *Service
	b       *svcBackend
	once    sync.Once
	BaseURL string
	ModelID string
	Target  *Target
	Waited  time.Duration
}

// Release returns the backend for the next request.
func (l *ServiceLease) Release() {
	l.once.Do(func() {
		l.s.mu.Lock()
		l.s.busy = false
		l.s.pump()
		l.s.mu.Unlock()
	})
}

// NewService builds a supervisor.
func NewService(d ServiceDeps) *Service {
	if d.QueueCapacity <= 0 {
		d.QueueCapacity = 16 // TS DEFAULT_MAX_WAITING_INFERENCES
	}
	if d.QueueTimeout <= 0 {
		d.QueueTimeout = 60 * time.Second // TS DEFAULT_INFERENCE_QUEUE_WAIT_MS
	}
	if d.Startup <= 0 {
		d.Startup = 60 * time.Second
	}
	return &Service{d: d}
}

// Resolve maps a requested model ("" = the active one) to a configured or
// imported id of this modality.
func (s *Service) Resolve(model string) (string, error) {
	cfg := s.d.Config()
	if model == "" {
		model = cfg.Active[s.d.Kind]
	}
	if model == "" {
		return "", ErrNotConfigured
	}
	for _, id := range ConfiguredIDs(cfg, s.d.Kind) {
		if id == model {
			return model, nil
		}
	}
	if s.d.Gate != nil {
		if t, _ := s.d.Gate(context.Background(), model); t != nil && t.Imported && t.Model.HasTask(taskOf(s.d.Modality)) {
			return model, nil
		}
	}
	return "", ErrModelNotFound
}

// ErrNotConfigured: the modality has no active model.
var ErrNotConfigured = errors.New("modality not configured")

func taskOf(modality string) string {
	return map[string]string{"stt": "transcription", "tts": "speech", "image": "image", "video": "video"}[modality]
}

// Acquire waits for exclusive use of modelID's backend, starting or
// switching it first.
func (s *Service) Acquire(ctx context.Context, modelID string) (*ServiceLease, error) {
	start := time.Now()
	key := ""
	if s.d.SelectionKey != nil {
		key = s.d.SelectionKey(modelID)
	}
	if s.d.ExtraKey != nil {
		key += "|" + s.d.ExtraKey()
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrUnavailable
	}
	if s.unloading == modelID {
		s.mu.Unlock()
		return nil, ErrUnloading
	}
	if len(s.queue) >= s.d.QueueCapacity {
		s.mu.Unlock()
		return nil, ErrQueueFull
	}
	w := newQueued[*ServiceLease](ctx, modelID, key)
	s.queue = append(s.queue, w)
	s.pump()
	s.mu.Unlock()
	lease, err := s.queue.wait(ctx, &s.mu, w, s.d.QueueTimeout, s.pump, (*ServiceLease).Release)
	if err != nil {
		return nil, err
	}
	lease.Waited = w.dispatched.Sub(start)
	return lease, nil
}

// pump dispatches the head of the queue. Called with mu held.
func (s *Service) pump() {
	if len(s.queue) > 0 && !s.busy && !s.claimed && !s.closed && s.unloading == "" {
		head := s.queue[0]
		if s.cur != nil && s.cur.modelID == head.model && s.cur.key == head.key {
			s.queue = s.queue[1:]
			s.busy = true
			head.done = true
			if head.dispatched.IsZero() {
				head.dispatched = time.Now()
			}
			head.lease = &ServiceLease{s: s, b: s.cur, BaseURL: s.cur.baseURL, ModelID: s.cur.modelID, Target: s.cur.target}
			head.ready <- nil
			return
		}
		if s.stopping {
			s.queue = s.queue[1:]
			head.done = true
			head.ready <- ErrUnavailable
			s.pump()
			return
		}
		s.busy = true // the switch holds the backend
		head.dispatching = true
		head.dispatched = time.Now()
		s.switching = true
		s.switchW.Add(1)
		ctx, cancel := context.WithCancelCause(context.Background())
		s.cancelSwitch = cancel
		go func(model, key string, parent *otlp.Span) {
			defer s.switchW.Done()
			defer cancel(nil)
			err := s.d.Recovery.retryAfterRecovery(ctx, s.d.Modality, func() (memory.Demand, error) {
				return s.doSwitch(ctx, model, key, parent)
			})
			s.mu.Lock()
			s.busy, s.switching = false, false
			s.cancelSwitch = nil
			s.failed = err != nil && s.cur == nil && ctx.Err() == nil // an eviction is not a failed start
			if err != nil {
				for len(s.queue) > 0 && s.queue[0].model == model && s.queue[0].key == key {
					w := s.queue[0]
					s.queue = s.queue[1:]
					w.done = true
					w.ready <- err
				}
			}
			s.pump()
			s.mu.Unlock()
		}(head.model, head.key, head.parent)
		return
	}
}

func (s *Service) doSwitch(ctx context.Context, modelID, key string, parent *otlp.Span) (demand memory.Demand, err error) {
	if s.d.Gate == nil {
		return demand, fmt.Errorf("%w: no backend selection", ErrStartupFailed)
	}
	t, err := s.d.Gate(ctx, modelID)
	if err != nil {
		return demand, err
	}
	demand = s.demand(t)
	// As TS activateModel: the switch must fit (crediting the running backend),
	// then the model becomes active, then it is installed and started. A start
	// that fails leaves it active. The legacy catalog variant keeps the TS
	// install check. Other variants were installed (or refused) by the gate.
	spec := s.d.Catalog.ByID(modelID)
	var installErr error
	if spec != nil && !t.Imported && t.Variant.ID == "default" {
		installErr = s.ensureInstalled(ctx, modelID)
	}
	previous := ""
	if s.d.Config != nil {
		previous = s.d.Config().Active[s.d.Kind]
	}
	switching := previous != modelID
	preflight := installErr == nil || preflightsFromCatalog(s.d.Modality, spec, t)
	if preflight {
		if err := s.d.Memory.CheckAdmission(context.Background(), demand, []string{s.d.Modality}); err != nil {
			if switching {
				logAdmissionRejected(s.d.Log, s.d.Modality, modalityComponents[s.d.Modality], err,
					"stage", "switch-preflight", "from_model", previous, "to_model", modelID)
			} else if s.d.Modality == "video" {
				// TS checks a stopped video runtime before starting it.
				logAdmissionRejected(s.d.Log, s.d.Modality, modalityComponents[s.d.Modality], err,
					"stage", "start-preflight", "model", modelID)
			} else {
				logAdmissionRejected(s.d.Log, s.d.Modality, modalityComponents[s.d.Modality], err)
			}
			return demand, err
		}
	}
	if s.d.Activate != nil && !t.Imported {
		s.d.Activate(s.d.Kind, modelID)
	}
	if installErr != nil {
		return demand, installErr
	}
	s.mu.Lock()
	prev := s.cur
	s.cur = nil
	s.mu.Unlock()
	if prev != nil {
		s.stop(prev, StopSwitch)
		if preflight && switching && previous != "" && ctx.Err() == nil {
			if err := settleAfterStop(ctx, s.d.Memory, demand); err != nil {
				if ctx.Err() != nil {
					return demand, context.Cause(ctx) // ended on purpose: nothing is rolled back
				}
				if s.rollBack(previous, modelID, err) {
					return demand, err
				}
			}
		}
	}
	if ctx.Err() != nil {
		return demand, context.Cause(ctx)
	}
	b, err := s.start(ctx, t, demand, parent)
	if err != nil {
		if ctx.Err() != nil {
			return demand, context.Cause(ctx)
		}
		return demand, err
	}
	b.key = key
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.stop(b, StopShutdown)
		return demand, ErrUnavailable
	}
	if ctx.Err() != nil {
		s.mu.Unlock()
		b.emergency.Store(true)
		s.stop(b, StopMemory)
		return demand, context.Cause(ctx)
	}
	s.cur = b
	s.mu.Unlock()
	s.watches.Add(1)
	go func() {
		defer s.watches.Done()
		s.watch(b)
	}()
	return demand, nil
}

// rollBack reinstates the previous active model after a switch whose memory
// never settled (TS rollBackRejectedSwitch). It reports false when the
// configuration moved on meanwhile (another model was activated or the
// previous one deselected); the switch then goes ahead. The previous model
// was running, so it is reloaded once memory admits it.
func (s *Service) rollBack(previous, requested string, err error) bool {
	cfg := s.d.Config()
	if s.d.Activate == nil || cfg.Active[s.d.Kind] != requested || !slices.Contains(cfg.Selected[s.d.Kind], previous) {
		return false
	}
	s.d.Activate(s.d.Kind, previous)
	logRolledBack(s.d.Log, s.d.Modality, previous, requested, true, err)
	s.mu.Lock()
	if !s.closed { // Close waits for the reload, and cancels it
		ctx := s.lifeLocked()
		s.reloads.Add(1)
		go func() {
			defer s.reloads.Done()
			s.reloadRestored(ctx, previous)
		}()
	}
	s.mu.Unlock()
	return true
}

// lifeLocked is a context Close cancels. s.mu is held.
func (s *Service) lifeLocked() context.Context {
	if s.life == nil {
		s.life, s.endLife = context.WithCancel(context.Background())
	}
	return s.life
}

// reloadRestored starts a rolled-back model once memory admits it, unless
// something else started meanwhile (TS reloadRestoredModel).
func (s *Service) reloadRestored(ctx context.Context, modelID string) {
	failed := func(err error) {
		s.d.Log.Warn("Restored model could not be reloaded; it will start on demand.", "event", "model.switch-restore-failed",
			"category", "runtime", "component", modalityComponents[s.d.Modality], "runtime", s.d.Modality,
			"error", err.Error(), slog.Group("attributes", "model", modelID))
	}
	t, err := s.d.Gate(ctx, modelID)
	if err != nil {
		failed(err)
		return
	}
	if err := settleAfterStop(ctx, s.d.Memory, s.demand(t)); err != nil {
		if ctx.Err() == nil { // Close ended it: nothing to report
			failed(err)
		}
		return
	}
	s.mu.Lock()
	idle := s.cur == nil && !s.switching && !s.closed && len(s.queue) == 0 && s.d.Config().Active[s.d.Kind] == modelID
	s.mu.Unlock()
	if !idle {
		return
	}
	l, err := s.Acquire(ctx, modelID)
	if err != nil {
		if ctx.Err() == nil {
			failed(err)
		}
		return
	}
	l.Release()
}

// preflightsFromCatalog reports whether the TS switch preflight sizes a
// catalog model whose files are missing, so the fit check (and a 503
// insufficient_memory) comes before the install. The TS image and stt
// preflights fall back to the catalog's primary artifact filename and
// expectedSizeBytes (supervisor-factory preflightDemand: "a missing catalog
// artifact is admitted on its catalog demand"), with or without an
// imageRuntime profile; the TS tts runtime has no switch preflight.
// tools/golden/media-admission.ts pins the demand and verdicts.
func preflightsFromCatalog(modality string, spec *catalog.Spec, t *Target) bool {
	return spec != nil && !t.Imported && (modality == "image" || modality == "stt")
}

func (s *Service) ensureInstalled(ctx context.Context, modelID string) error {
	if ok, _ := s.d.Catalog.ByID(modelID).Installation(s.d.ModelsDir); ok {
		return nil
	}
	if s.d.Install == nil {
		return &NotInstalledError{ModelID: modelID}
	}
	return s.d.Install(ctx, modelID)
}

// demand is the memory the backend is admitted with: its estimate, or what it has been seen to
// need if that is more.
func (s *Service) demand(t *Target) memory.Demand {
	d := s.estimatedDemand(t)
	spec := s.d.Catalog.ByID(t.ModelID)
	if spec == nil || t.Imported || spec.Primary().SHA256 == "" || s.d.Store == nil {
		return d
	}
	peaks, err := s.d.Store.ObservedPeaks(context.Background(), t.ModelID, spec.Primary().SHA256)
	if err != nil {
		s.d.Log.Warn("Could not read what this model was seen to need; using its estimate.",
			"event", "memory.observed-read-failed", "model", t.ModelID, "error", err.Error())
		return d
	}
	return RaiseToObserved(d, peaks, mediaShape(t.Backend.ID))
}

// estimatedDemand is the catalog estimate for catalog models, the declared rule for custom backends,
// otherwise the weights plus a fixed runtime allowance (an estimate, labelled so).
func (s *Service) estimatedDemand(t *Target) memory.Demand {
	if t.Backend.Adapter == "process-openai/v1" && t.Backend.Launch != nil && t.Backend.Launch.Memory != nil {
		weights := t.weightsBytes()
		m := t.Backend.Launch.Memory
		b := int64(float64(weights)*m.WeightsFactor) + int64(m.OverheadGB*float64(memory.GiB))
		return memory.Demand{UnifiedBytes: b, HostBytes: b, AcceleratorBytes: b, Confidence: "declared"}
	}
	if spec := s.d.Catalog.ByID(t.ModelID); spec != nil && !t.Imported {
		if d := catalogServiceDemand(s.d.Catalog, spec, t.weightsBytes()); d.UnifiedBytes > 0 {
			return d
		}
	}
	b := t.weightsBytes() + memory.GiB
	return memory.Demand{UnifiedBytes: b, HostBytes: b, AcceleratorBytes: b, Confidence: "estimated"}
}

func (s *Service) start(life context.Context, t *Target, demand memory.Demand, parent *otlp.Span) (*svcBackend, error) {
	ctx := otlp.WithSpan(context.Background(), parent)
	fail := func(err error) error {
		logStartFailed(s.d.Log, s.d.Modality, err)
		if t.Manual || t.Imported {
			return &BackendStartError{BackendID: t.Backend.ID, Cause: err}
		}
		return fmt.Errorf("%w: %v", ErrStartupFailed, err)
	}
	if !runsHere(t.Backend) {
		return nil, fail(fmt.Errorf("%s has no build for %s/%s", t.Backend.Name, goruntime.GOOS, goruntime.GOARCH))
	}
	host, port := BackendHost(s.d.Host), s.d.Port
	cmd, err := s.command(life, t, host, port)
	if err != nil {
		var launch *LaunchError
		if errors.As(err, &launch) {
			return nil, err
		}
		return nil, fail(err)
	}
	if err := checkPortFree(host, port); err != nil {
		return nil, fail(err)
	}
	res, err := s.d.Memory.Reserve(life, s.d.Modality, demand)
	if err != nil {
		if life.Err() != nil {
			return nil, context.Cause(life) // ended on purpose, not refused for memory
		}
		logAdmissionRejected(s.d.Log, s.d.Modality, cmd.name, err)
		return nil, err
	}
	s.d.Log.InfoContext(ctx, "Starting "+cmd.name+" for "+t.ModelID+".", "event", "runtime.starting", "runtime", s.d.Modality,
		"model_id", t.ModelID, "backend_id", t.Backend.ID, "demand_bytes", demand.UnifiedBytes, "demand_confidence", demand.Confidence)
	var proc *Process
	err = spanned(s.d.Otel, ctx, "localbase.backend.start", cmd.name, func() (err error) {
		proc, err = StartProcessIn(cmd.name, cmd.bin, cmd.dir, cmd.args, cmd.env, s.d.Log)
		return err
	})
	if err != nil {
		res.Release()
		return nil, fail(err)
	}
	recordBackend(s.d.RootDir, s.d.Modality, port, proc, s.d.Log)
	proc.ForeignListenerOK = t.Backend.Launch != nil
	base := "http://" + net.JoinHostPort(host, strconv.Itoa(port))
	budget := s.d.Startup
	if cmd.startup > 0 {
		budget = cmd.startup
	}
	if err := spanned(s.d.Otel, ctx, "localbase.backend.model_load", cmd.name, func() error {
		return proc.WaitHealthy(life, base+cmd.health, budget)
	}); err != nil {
		proc.Stop(loadStopGrace)
		res.Release()
		return nil, fail(err)
	}
	res.Materialize(proc.PID())
	s.d.Log.InfoContext(ctx, cmd.name+" is ready for "+t.ModelID+".", "event", "runtime.ready", "runtime", s.d.Modality, "model_id", t.ModelID, "pid", proc.PID())
	sampling, stopSampling := context.WithCancel(context.Background())
	return &svcBackend{modelID: t.ModelID, target: t, proc: proc, res: res, baseURL: base, sampling: sampling, stopSampling: stopSampling}, nil
}

func (s *Service) stop(b *svcBackend, reason string) {
	b.stopSampling()
	b.proc.Stop(stopGrace)
	logStopped(s.d.Log, s.d.Modality, b.modelID, reason, b.res.Bytes(), b.proc.PID())
	b.res.Release()
}

// watch clears the slot when the backend exits on its own; the next request
// starts it again.
func (s *Service) watch(b *svcBackend) {
	defer b.stopSampling()
	// The observed peak footprint of catalog models (macOS), as for the LLM, by backend: the same
	// file can need different memory on another one.
	spec := s.d.Catalog.ByID(b.modelID)
	record := spec != nil && !b.target.Imported && spec.Primary().SHA256 != "" && goruntime.GOOS == "darwin"
	var peak int64
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
			if err := s.d.Store.RecordObservedPeak(context.Background(), store.ObservedPeak{ModelID: b.modelID,
				ArtifactSHA256: spec.Primary().SHA256, LaunchShape: mediaShape(b.target.Backend.ID), PeakBytes: bytes, Method: "footprint"}); err != nil {
				s.d.Store.WriteFailed("observed-peak", err)
			} else {
				peak = bytes
			}
		}
	}
	sample()
	tick := time.NewTicker(samplePeriod)
	defer tick.Stop()
wait:
	for {
		select {
		case <-b.proc.Done():
			break wait
		case <-tick.C:
			sample()
		}
	}
	s.mu.Lock()
	if s.cur == b {
		s.cur = nil
		s.d.Log.Warn("The "+s.d.Modality+" backend exited; the next request starts it again.", "event", "runtime.exited",
			"runtime", s.d.Modality, "model_id", b.modelID, "error", fmt.Sprint(b.proc.ExitErr()))
		logStopped(s.d.Log, s.d.Modality, b.modelID, StopCrash, b.res.Bytes(), b.proc.PID())
		b.res.Release()
	}
	s.mu.Unlock()
}

// PreviewAdmission is what a request for id would meet now, computed as doSwitch
// does, changing nothing and without installing. A memory refusal comes before a
// missing install where the real path preflights from the catalog (TS).
// assumeInstalled answers the memory question alone, for a model not yet on disk.
func (s *Service) PreviewAdmission(ctx context.Context, id string, assumeInstalled bool) (memory.AdmissionPreview, memory.Demand, error) {
	var demand memory.Demand
	if s.d.Gate == nil {
		return memory.AdmissionPreview{}, demand, fmt.Errorf("%w: no backend selection", ErrStartupFailed)
	}
	t, err := s.d.Gate(ctx, id)
	if err != nil {
		return memory.AdmissionPreview{}, demand, err
	}
	demand = s.demand(t)
	spec := s.d.Catalog.ByID(id)
	var missing error
	if spec != nil && !t.Imported && t.Variant.ID == "default" && !assumeInstalled {
		if ok, _ := spec.Installation(s.d.ModelsDir); !ok {
			missing = &NotInstalledError{ModelID: id}
		}
	}
	if missing != nil && !preflightsFromCatalog(s.d.Modality, spec, t) {
		return memory.AdmissionPreview{}, demand, missing
	}
	p := s.d.Memory.Preview(ctx, demand, append([]string{s.d.Modality}, s.d.Recovery.idleIDs(s.d.Modality)...))
	if s.Running() == id {
		p.Err = nil // already loaded: nothing to admit
	}
	if p.Err == nil && missing != nil {
		return p, demand, missing
	}
	return p, demand, nil
}

// Activity is the request count behind Busy: a backend runs one at a time.
func (s *Service) Activity() (inFlight, queued int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy && !s.switching {
		inFlight = 1
	}
	return inFlight, len(s.queue)
}

// Running reports the resident model.
// Snapshot reports the service for /health (the LLM's Snapshot fields).
func (s *Service) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := Snapshot{Waiting: len(s.queue), Capacity: s.d.QueueCapacity, Accepting: !s.closed, State: "idle"}
	switch {
	case s.closed:
		snap.State = "stopping"
	case s.switching:
		snap.State = "starting"
	case s.cur != nil:
		snap.State, snap.ModelID = "running", s.cur.modelID
	}
	return snap
}

func (s *Service) Running() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return ""
	}
	return s.cur.modelID
}

// EmergencyStop rejects queued work with reason and stops the backend, busy
// or not (memory critical); the next request starts it again.
func (s *Service) EmergencyStop(reason error) {
	s.mu.Lock()
	for _, w := range s.queue {
		w.done = true
		w.ready <- reason
	}
	s.queue = nil
	if s.cancelSwitch != nil {
		s.cancelSwitch(reason)
	}
	b := s.cur
	s.cur = nil
	s.mu.Unlock()
	if b != nil {
		b.emergency.Store(true)
		s.stop(b, StopMemory)
	}
}

// Close stops the backend after in-flight switches finish.
// StopLoads ends a load in flight and starts no other, as on the LLM.
func (s *Service) StopLoads() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopping = true
	if s.cancelSwitch != nil {
		s.cancelSwitch(ErrUnavailable)
	}
	s.pump()
}

func (s *Service) Close() {
	s.mu.Lock()
	s.closed = true
	for _, w := range s.queue {
		w.done = true
		w.ready <- ErrUnavailable
	}
	s.queue = nil
	if s.cancelSwitch != nil {
		s.cancelSwitch(ErrUnavailable)
	}
	if s.endLife != nil {
		s.endLife()
	}
	s.mu.Unlock()
	s.reloads.Wait()
	s.switchW.Wait()
	s.mu.Lock()
	b := s.cur
	s.cur = nil
	s.mu.Unlock()
	if b != nil {
		s.stop(b, StopShutdown)
	}
	s.watches.Wait()
}

// --- adapters --------------------------------------------------------------------------

type serviceCommand struct {
	name, bin, dir string
	args, env      []string
	health         string
	startup        time.Duration // 0: the service default
}

func (s *Service) binary(life context.Context, name string, t *Target) (string, error) {
	if t.Backend.Launch != nil && t.Backend.Launch.Command != "" {
		return t.Backend.Launch.Command, nil
	}
	if b := s.d.Binaries[name]; b != "" {
		return b, nil
	}
	if err := life.Err(); err != nil {
		return "", err // an ended switch says so, whether or not this platform has a pin
	}
	return EnsureBinary(life, s.d.RuntimesDir, name)
}

func (t *Target) artifact(role string) string {
	for _, a := range t.Variant.Artifacts {
		if a.Role == role {
			return filepath.Join(t.Dir, a.Filename)
		}
	}
	return ""
}

func (t *Target) weightsBytes() int64 {
	var n int64
	for _, a := range t.Variant.Artifacts {
		if a.Role == models.RoleWeights {
			n += fileSize(filepath.Join(t.Dir, a.Filename))
		}
	}
	return n
}

// command builds the backend command for the target's adapter.
func (s *Service) command(life context.Context, t *Target, host string, port int) (*serviceCommand, error) {
	d := t.Backend
	weights := ""
	if w, err := t.weights(); err == nil {
		weights = filepath.Join(t.Dir, w)
	}
	switch d.Adapter {
	case "whisper-server/v1":
		if s.d.Modality != "stt" {
			break
		}
		bin, err := s.binary(life, "whisper-server", t)
		if err != nil {
			return nil, fmt.Errorf("%s is not installed on this device: %v", d.Name, err)
		}
		return &serviceCommand{name: "whisper-server", bin: bin, health: "/health",
			args: []string{"--model", weights, "--host", host, "--port", strconv.Itoa(port)}}, nil
	case "crispasr/v1":
		if s.d.Modality != "tts" {
			break
		}
		return s.crispasrCommand(life, t, host, port, weights)
	case "tts-server/v1":
		if s.d.Modality != "tts" {
			break
		}
		return s.ttsServerCommand(life, t, host, port, weights)
	case "sd-server/v1":
		if s.d.Modality == "video" {
			return s.sdVideoCommand(life, t, host, port)
		}
		if s.d.Modality != "image" {
			break
		}
		return s.sdServerCommand(life, t, host, port, weights)
	case "process-openai/v1":
		l := d.Launch
		repl := strings.NewReplacer("{model}", weights, "{model_dir}", t.Dir, "{host}", host, "{port}", strconv.Itoa(port), "{ctx}", "0", "{projector}", "")
		var args []string
		for _, a := range l.Args {
			args = append(args, repl.Replace(a))
		}
		health := l.HealthPath
		if health == "" {
			health = "/health"
		}
		return &serviceCommand{name: d.ID, bin: l.Command, args: args, health: health}, nil
	}
	return nil, fmt.Errorf("%s (adapter %s) does not run %s models", d.Name, d.Adapter, taskOf(s.d.Modality))
}

// catalogServiceDemand is a catalog media model's admission demand (TS
// catalogDemand then admissionDemand): the checked-in measurement, else a
// feed peak for exactly this file and launch, else the estimate; a local
// file larger than the catalog's adds its extra bytes.
func catalogServiceDemand(cat *catalog.Catalog, spec *catalog.Spec, localBytes int64) memory.Demand {
	catalogBytes := spec.TotalArtifactBytes()
	d, measured := memory.MeasuredCatalog(spec, nil)
	if measured {
		d.Confidence = "measured"
	} else if p := memory.SelectFeedPeak(spec, cat.PlannableFeedPeaks(spec), nil); p != nil {
		d = memory.FeedDemand(spec, *p, catalogBytes, 0)
	} else {
		d = memory.EstimateCatalog(spec, nil)
	}
	if extra := localBytes - catalogBytes; extra > 0 && d.UnifiedBytes > 0 {
		d.UnifiedBytes += extra
		d.HostBytes += extra
		d.AcceleratorBytes += extra
	}
	return d
}

// Interrupted reports whether the lease's runtime was stopped because memory
// turned critical; the request then answers 503 with Retry-After.
func (l *ServiceLease) Interrupted() bool { return l.b != nil && l.b.emergency.Load() }
