package runtime

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/otlp"
	"github.com/timsexperiments/LocalBase/go/internal/placement"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

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
	res, err := m.d.Memory.Reserve(life, "llm", demand)
	if err != nil {
		if life.Err() != nil {
			return nil, context.Cause(life) // ended on purpose, not refused for memory
		}
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
