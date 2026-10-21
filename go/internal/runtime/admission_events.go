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
	log.Warn(message, "event", "runtime.memory-admission-rejected", "category", "runtime",
		"component", component, "runtime", modality,
		slog.Group("attributes", append(ae.Attributes(), context...)...))
}

// The post-stop settle (TS settleAdmissionAfterEviction); tests shorten it.
var (
	postStopSettle   = memorySettleTimeout
	postStopInterval = memorySettleInterval
)

// settleAfterStop waits for memory to admit demand after the previous model
// stopped: the OS can return reclaimed pages more slowly than the switch
// preflight assumed. It samples every 100 ms for up to 10 s; a permanent
// shortfall answers at once. Only memory refusals are returned.
func settleAfterStop(mem *memory.Controller, demand memory.Demand) error {
	deadline := time.Now().Add(postStopSettle)
	for {
		err := mem.CheckAdmission(context.Background(), demand, nil)
		var ae *memory.AdmissionError
		if !errors.As(err, &ae) {
			return nil
		}
		if ae.Permanent || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(min(postStopInterval, time.Until(deadline)+time.Millisecond))
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
