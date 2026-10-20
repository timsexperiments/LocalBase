package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Service supervises the one resident backend of a non-LLM modality (STT,
// TTS, image): a FIFO queue, memory admission before start, model switches,
// and crash recovery on the next request. Requests run one at a time, as in
// TS. The backend comes from the device's selection (M3.5); the adapter
// turns it into a command line.
type Service struct {
	d ServiceDeps

	mu      sync.Mutex
	cur     *svcBackend
	busy    bool
	queue   []*svcWaiter
	closed  bool
	failed  bool
	switchW sync.WaitGroup
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
	ready      chan error
	lease      *ServiceLease
	done       bool
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
		d.QueueCapacity = 8
	}
	if d.QueueTimeout <= 0 {
		d.QueueTimeout = 120 * time.Second
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
	w := &svcWaiter{model: modelID, key: key, ready: make(chan error, 1)}
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
		w.lease.Waited = time.Since(start)
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
	for len(s.queue) > 0 && !s.busy && !s.closed {
		head := s.queue[0]
		if s.cur != nil && s.cur.modelID == head.model && s.cur.key == head.key {
			s.queue = s.queue[1:]
			s.busy = true
			head.done = true
			head.lease = &ServiceLease{s: s, b: s.cur, BaseURL: s.cur.baseURL, ModelID: s.cur.modelID, Target: s.cur.target}
			head.ready <- nil
			return
		}
		s.busy = true // the switch holds the backend
		s.switchW.Add(1)
		go func(model, key string) {
			defer s.switchW.Done()
			err := s.doSwitch(model, key)
			s.mu.Lock()
			s.busy = false
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
		}(head.model, head.key)
		return
	}
}

func (s *Service) doSwitch(modelID, key string) error {
	if s.d.Gate == nil {
		return fmt.Errorf("%w: no backend selection", ErrStartupFailed)
	}
	t, err := s.d.Gate(modelID)
	if err != nil {
		return err
	}
	// The legacy catalog variant keeps the TS install check; other variants
	// were installed (or refused) by the gate.
	if s.d.Catalog.ByID(modelID) != nil && !t.Imported && t.Variant.ID == "default" {
		if err := s.ensureInstalled(modelID); err != nil {
			return err
		}
	}
	demand := s.demand(t)
	s.mu.Lock()
	prev := s.cur
	s.cur = nil
	s.mu.Unlock()
	if prev != nil {
		s.stop(prev)
	}
	b, err := s.start(t, demand)
	if err != nil {
		return err
	}
	b.key = key
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.stop(b)
		return ErrUnavailable
	}
	s.cur = b
	s.mu.Unlock()
	go s.watch(b)
	if s.d.Activate != nil && !t.Imported {
		s.d.Activate(s.d.Kind, modelID)
	}
	return nil
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
		if d := memory.EstimateCatalog(spec, nil); d.UnifiedBytes > 0 {
			return d
		}
	}
	b := t.weightsBytes() + memory.GiB
	return memory.Demand{UnifiedBytes: b, HostBytes: b, AcceleratorBytes: b, Confidence: "estimated"}
}

func (s *Service) start(t *Target, demand memory.Demand) (*svcBackend, error) {
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
		return nil, err
	}
	s.d.Log.Info("Starting "+cmd.name+" for "+t.ModelID+".", "event", "runtime.starting", "runtime", s.d.Modality,
		"model_id", t.ModelID, "backend_id", t.Backend.ID, "demand_bytes", demand.UnifiedBytes, "demand_confidence", demand.Confidence)
	proc, err := StartProcessIn(cmd.name, cmd.bin, cmd.dir, cmd.args, cmd.env, s.d.Log)
	if err != nil {
		res.Release()
		return nil, fail(err)
	}
	res.Materialize(proc.PID())
	base := "http://" + net.JoinHostPort(host, strconv.Itoa(port))
	if err := proc.WaitHealthy(context.Background(), base+cmd.health, s.d.Startup); err != nil {
		proc.Stop(stopGrace)
		res.Release()
		return nil, fail(err)
	}
	s.d.Log.Info(cmd.name+" is ready for "+t.ModelID+".", "event", "runtime.ready", "runtime", s.d.Modality, "model_id", t.ModelID, "pid", proc.PID())
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
func (s *Service) Running() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return ""
	}
	return s.cur.modelID
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
