package runtime

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

// modalityComponents name each modality's component in reconciler events
// (TS modalityComponents).
var modalityComponents = map[string]string{"llm": "llama-server", "stt": "whisper-server", "tts": "llama-tts",
	"image": "sd-server", "video": "sd-server"}

// logAdmissionRejected is TS runtime.memory-admission-rejected: a memory
// refusal with its diagnostics (reason, pool, the measured, reserve,
// pending, requested and effective bytes, the sample, the safety state and
// the demand confidence), then the stage context: none for a start (the
// supervisor's event), or stage switch-preflight or post-stop with
// from_model and to_model.
func logAdmissionRejected(log *slog.Logger, modality, component string, err error, context ...any) {
	var ae *memory.AdmissionError
	if log == nil || !errors.As(err, &ae) {
		return
	}
	message := "Insufficient memory to start the requested runtime."
	if ae.PoolID == "gpu-working-set" && !ae.Permanent {
		message = "Insufficient GPU working set to start the requested runtime alongside the runtimes already loaded."
	}
	attrs := append(ae.Attributes(), context...)
	if ae.NVIDIAPresent {
		// On an NVIDIA host, GPU memory is admitted in the separate gpu:0
		// pool for discrete placements (DESIGN §20.4).
		attrs = append(attrs, "nvidia_gpu_present", true, "vram_admission", "gpu:0")
	}
	log.Warn(message, "event", "runtime.memory-admission-rejected", "category", "runtime",
		"component", component, "runtime", modality,
		slog.Group("attributes", attrs...))
}

// hooks are what a runtime's tests change: how it reads a backend's memory, how often, and the
// post-stop settle (TS settleAdmissionAfterEviction). The zero value is how the package runs.
type hooks struct {
	// sample reads a backend's memory (tests: one that holds, or fixed figures); nil is
	// memory.Footprint.
	sample func(ctx context.Context, pid int) (int64, bool)
	// samplePeriod is how often a running model's memory is sampled; 0 is samplePeriod.
	samplePeriod time.Duration
	// settle and settleStep are the post-stop settle's limit and step; 0 is memorySettleTimeout
	// and memorySettleInterval.
	settle, settleStep time.Duration
}

func (h hooks) footprint(ctx context.Context, pid int) (int64, bool) {
	if h.sample != nil {
		return h.sample(ctx, pid)
	}
	return memory.Footprint(ctx, pid)
}

func (h hooks) period() time.Duration {
	if h.samplePeriod > 0 {
		return h.samplePeriod
	}
	return samplePeriod
}

func (h hooks) settleLimit() time.Duration {
	if h.settle > 0 {
		return h.settle
	}
	return memorySettleTimeout
}

func (h hooks) settleInterval() time.Duration {
	if h.settleStep > 0 {
		return h.settleStep
	}
	return memorySettleInterval
}

// settleAfterStop waits for memory to admit demand after the previous model
// stopped: the OS can return reclaimed pages more slowly than the switch
// preflight assumed. It samples every 100 ms for up to 10 s; a permanent
// shortfall answers at once. Only memory refusals are returned.
func settleAfterStop(ctx context.Context, mem *memory.Controller, demand memory.Demand, h hooks) error {
	deadline := time.Now().Add(h.settleLimit())
	for {
		if ctx.Err() != nil { // the switch was ended (Close, an emergency stop): stop waiting
			return context.Cause(ctx)
		}
		err := mem.CheckAdmission(ctx, demand, nil)
		var ae *memory.AdmissionError
		if !errors.As(err, &ae) {
			return nil
		}
		if ae.Permanent || !time.Now().Before(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
		case <-time.After(min(h.settleInterval(), time.Until(deadline)+time.Millisecond)):
		}
	}
}

// logRolledBack records a switch undone because memory did not settle
// (TS model.switch-rolled-back), after the post-stop refusal.
func logRolledBack(log *slog.Logger, modality, from, to string, reload bool, err error) {
	logAdmissionRejected(log, modality, modalityComponents[modality], err,
		"stage", "post-stop", "from_model", from, "to_model", to)
	if log == nil {
		return
	}
	log.Warn("Memory did not settle after stopping the previous model; restored it.", "event", "model.switch-rolled-back",
		"category", "runtime", "component", modalityComponents[modality], "runtime", modality,
		slog.Group("attributes", "from_model", from, "to_model", to, "reload", reload))
}

// logStartFailed is TS backend.start-failed: a backend that could not start,
// with the cause.
func logStartFailed(log *slog.Logger, modality string, err error) {
	if log == nil {
		return
	}
	log.Error("Backend startup failed.", "event", "backend.start-failed", "category", "runtime",
		"component", modalityComponents[modality], "runtime", modality, "error", err.Error())
}

// Why a runtime's backend stopped (runtime.stopped and runtime.evicted).
const (
	StopMemory   = "memory"   // evicted for memory: idle recovery, pressure, critical memory
	StopSwitch   = "switch"   // replaced by another model
	StopShutdown = "shutdown" // the gateway is stopping
	StopCrash    = "crash"    // the backend exited on its own (runtime.exited too)
	StopCancel   = "cancel"   // a job was cancelled (video)
	StopIdle     = "idle"     // unloaded after idling (no such timer yet)
	StopUnload   = "unload"   // explicitly unloaded (POST /_localbase/models/{id}/unload)
)

// logStopped records a backend stop: runtime.evicted for memory, else
// runtime.stopped, with the reserved memory it freed (operator observability
// audit, DESIGN §13.6 note on events).
func logStopped(log *slog.Logger, runtime, modelID, reason string, freed int64, pid int) {
	if log == nil {
		return
	}
	event, msg := "runtime.stopped", "Stopped the "+runtime+" runtime ("+reason+")."
	if reason == StopMemory {
		event, msg = "runtime.evicted", "Evicted the "+runtime+" runtime for memory."
	}
	log.Info(msg, "event", event, "runtime", runtime, "model_id", modelID, "reason", reason,
		"memory_freed_bytes", freed, "pid", pid)
}
