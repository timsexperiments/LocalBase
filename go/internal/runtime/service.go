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
	queue     []*svcWaiter
	closed    bool
	failed    bool
	switching bool // a model is being stopped or started
	claimed   bool // idle recovery is stopping the backend; hold requests
	switchW   sync.WaitGroup
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
}

type svcWaiter struct {
	model, key string
	parent     *otlp.Span // the request's span, for a start it triggers
	ready      chan error
	lease      *ServiceLease
	done       bool
	dispatched time.Time // left the queue (TS queueWaitMs)
}

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
		if t, _ := s.d.Gate(model); t != nil && t.Imported && t.Model.HasTask(taskOf(s.d.Modality)) {
			return model, nil
		}
	}
	return "", ErrModelNotFound
}

// ErrNotConfigured: the modality has no active model.
var ErrNotConfigured = errors.New("modality not configured")

func taskOf(modality string) string {
	return map[string]string{"stt": "transcription", "tts": "speech", "image": "image"}[modality]
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
	if len(s.queue) >= s.d.QueueCapacity {
		s.mu.Unlock()
		return nil, ErrQueueFull
	}
	w := &svcWaiter{model: modelID, key: key, ready: make(chan error, 1), parent: otlp.FromContext(ctx)}
	s.queue = append(s.queue, w)
	s.pump()
	s.mu.Unlock()
	timer := time.NewTimer(s.d.QueueTimeout)
	defer timer.Stop()
	select {
	case err := <-w.ready:
		if err != nil {
			return nil, err
		}
		w.lease.Waited = w.dispatched.Sub(start)
		return w.lease, nil
	case <-ctx.Done():
		s.abandon(w)
		return nil, ctx.Err()
	case <-timer.C:
		s.mu.Lock()
		if w.done {
			s.mu.Unlock()
			if err := <-w.ready; err != nil {
				return nil, err
			}
			return w.lease, nil
		}
		s.remove(w)
		s.mu.Unlock()
		return nil, ErrQueueTimeout
	}
}

func (s *Service) remove(w *svcWaiter) {
	for i, q := range s.queue {
		if q == w {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			return
		}
	}
}

func (s *Service) abandon(w *svcWaiter) {
	s.mu.Lock()
	granted := w.done && w.lease != nil
	s.remove(w)
	s.pump()
	s.mu.Unlock()
	if granted {
		select {
		case <-w.ready:
		default:
		}
		w.lease.Release()
	}
}

// pump dispatches the head of the queue. Called with mu held.
func (s *Service) pump() {
	for len(s.queue) > 0 && !s.busy && !s.claimed && !s.closed {
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
		s.busy = true // the switch holds the backend
		head.dispatched = time.Now()
		s.switching = true
		s.switchW.Add(1)
		go func(model, key string, parent *otlp.Span) {
			defer s.switchW.Done()
			err := s.d.Recovery.retryAfterRecovery(s.d.Modality, func() (memory.Demand, error) {
				return s.doSwitch(model, key, parent)
			})
			s.mu.Lock()
			s.busy, s.switching = false, false
			s.failed = err != nil && s.cur == nil
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

func (s *Service) doSwitch(modelID, key string, parent *otlp.Span) (demand memory.Demand, err error) {
	if s.d.Gate == nil {
		return demand, fmt.Errorf("%w: no backend selection", ErrStartupFailed)
	}
	t, err := s.d.Gate(modelID)
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
		installErr = s.ensureInstalled(modelID)
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
		s.stop(prev)
		if preflight && switching && previous != "" {
			if err := settleAfterStop(s.d.Memory, demand); err != nil && s.rollBack(previous, modelID, err) {
				return demand, err
			}
		}
	}
	b, err := s.start(t, demand, parent)
	if err != nil {
		return demand, err
	}
	b.key = key
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.stop(b)
		return demand, ErrUnavailable
	}
	s.cur = b
	s.mu.Unlock()
	go s.watch(b)
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
	go s.reloadRestored(previous)
	return true
}

// reloadRestored starts a rolled-back model once memory admits it, unless
// something else started meanwhile (TS reloadRestoredModel).
func (s *Service) reloadRestored(modelID string) {
	failed := func(err error) {
		s.d.Log.Warn("Restored model could not be reloaded; it will start on demand.", "event", "model.switch-restore-failed",
			"category", "runtime", "component", modalityComponents[s.d.Modality], "runtime", s.d.Modality,
			"error", err.Error(), slog.Group("attributes", "model", modelID))
	}
	t, err := s.d.Gate(modelID)
	if err != nil {
		failed(err)
		return
	}
	if err := settleAfterStop(s.d.Memory, s.demand(t)); err != nil {
		failed(err)
		return
	}
	s.mu.Lock()
	idle := s.cur == nil && !s.switching && !s.closed && len(s.queue) == 0 && s.d.Config().Active[s.d.Kind] == modelID
	s.mu.Unlock()
	if !idle {
		return
	}
	l, err := s.Acquire(context.Background(), modelID)
	if err != nil {
		failed(err)
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

func (s *Service) ensureInstalled(modelID string) error {
	if ok, _ := s.d.Catalog.ByID(modelID).Installation(s.d.ModelsDir); ok {
		return nil
	}
	if s.d.Install == nil {
		return &NotInstalledError{ModelID: modelID}
	}
	return s.d.Install(context.Background(), modelID)
}

// demand is the memory the backend is admitted with: the catalog estimate
// for catalog models, the declared rule for custom backends, otherwise the
// weights plus a fixed runtime allowance (an estimate, labelled so).
func (s *Service) demand(t *Target) memory.Demand {
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

func (s *Service) start(t *Target, demand memory.Demand, parent *otlp.Span) (*svcBackend, error) {
	ctx := otlp.WithSpan(context.Background(), parent)
	fail := func(err error) error {
		if t.Manual || t.Imported {
			return &BackendStartError{BackendID: t.Backend.ID, Cause: err}
		}
		return fmt.Errorf("%w: %v", ErrStartupFailed, err)
	}
	if !runsHere(t.Backend) {
		return nil, fail(fmt.Errorf("%s has no build for %s/%s", t.Backend.Name, goruntime.GOOS, goruntime.GOARCH))
	}
	host, port := BackendHost(s.d.Host), s.d.Port
	cmd, err := s.command(t, host, port)
	if err != nil {
		return nil, fail(err)
	}
	res, err := s.d.Memory.Reserve(context.Background(), s.d.Modality, demand)
	if err != nil {
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
	res.Materialize(proc.PID())
	base := "http://" + net.JoinHostPort(host, strconv.Itoa(port))
	budget := s.d.Startup
	if cmd.startup > 0 {
		budget = cmd.startup
	}
	if err := spanned(s.d.Otel, ctx, "localbase.backend.model_load", cmd.name, func() error {
		return proc.WaitHealthy(context.Background(), base+cmd.health, budget)
	}); err != nil {
		proc.Stop(stopGrace)
		res.Release()
		return nil, fail(err)
	}
	s.d.Log.InfoContext(ctx, cmd.name+" is ready for "+t.ModelID+".", "event", "runtime.ready", "runtime", s.d.Modality, "model_id", t.ModelID, "pid", proc.PID())
	return &svcBackend{modelID: t.ModelID, target: t, proc: proc, res: res, baseURL: base}, nil
}

func (s *Service) stop(b *svcBackend) {
	b.proc.Stop(stopGrace)
	b.res.Release()
}

// watch clears the slot when the backend exits on its own; the next request
// starts it again.
func (s *Service) watch(b *svcBackend) {
	// The observed peak footprint of catalog models (macOS), as for the LLM;
	// non-LLM launches use the "default" shape.
	spec := s.d.Catalog.ByID(b.modelID)
	record := spec != nil && !b.target.Imported && spec.Primary().SHA256 != "" && goruntime.GOOS == "darwin"
	var peak int64
	sample := func() {
		if !record {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		bytes, ok := memory.Footprint(ctx, b.proc.PID())
		cancel()
		if ok && bytes > peak {
			peak = bytes
			_ = s.d.Store.RecordObservedPeak(context.Background(), store.ObservedPeak{ModelID: b.modelID,
				ArtifactSHA256: spec.Primary().SHA256, LaunchShape: "default", PeakBytes: bytes, Method: "footprint"})
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
		b.res.Release()
		s.d.Log.Warn("The "+s.d.Modality+" backend exited; the next request starts it again.", "event", "runtime.exited",
			"runtime", s.d.Modality, "model_id", b.modelID)
	}
	s.mu.Unlock()
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
	b := s.cur
	s.cur = nil
	s.mu.Unlock()
	if b != nil {
		s.stop(b)
	}
}

// Close stops the backend after in-flight switches finish.
func (s *Service) Close() {
	s.mu.Lock()
	s.closed = true
	for _, w := range s.queue {
		w.done = true
		w.ready <- ErrUnavailable
	}
	s.queue = nil
	s.mu.Unlock()
	s.switchW.Wait()
	s.mu.Lock()
	b := s.cur
	s.cur = nil
	s.mu.Unlock()
	if b != nil {
		s.stop(b)
	}
}

// --- adapters --------------------------------------------------------------------------

type serviceCommand struct {
	name, bin, dir string
	args, env      []string
	health         string
	startup        time.Duration // 0: the service default
}

func (s *Service) binary(name string, t *Target) (string, error) {
	if t.Backend.Launch != nil && t.Backend.Launch.Command != "" {
		return t.Backend.Launch.Command, nil
	}
	if b := s.d.Binaries[name]; b != "" {
		return b, nil
	}
	return EnsureBinary(context.Background(), s.d.RuntimesDir, name)
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
func (s *Service) command(t *Target, host string, port int) (*serviceCommand, error) {
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
		bin, err := s.binary("whisper-server", t)
		if err != nil {
			return nil, fmt.Errorf("%s is not installed on this device: %v", d.Name, err)
		}
		return &serviceCommand{name: "whisper-server", bin: bin, health: "/health",
			args: []string{"--model", weights, "--host", host, "--port", strconv.Itoa(port)}}, nil
	case "crispasr/v1":
		if s.d.Modality != "tts" {
			break
		}
		return s.crispasrCommand(t, host, port, weights)
	case "tts-server/v1":
		if s.d.Modality != "tts" {
			break
		}
		return s.ttsServerCommand(t, host, port, weights)
	case "sd-server/v1":
		if s.d.Modality != "image" {
			break
		}
		return s.sdServerCommand(t, host, port, weights)
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
