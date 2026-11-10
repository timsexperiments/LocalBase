package video

import (
	"errors"
	"fmt"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
)

// FreeMemoryHint is what to do when a video can't start for lack of memory.
const FreeMemoryHint = "Free memory from a loaded model or close other apps, then try again."

// FailureError names a failed job's error code (TS videoFailureError).
func FailureError(err error) map[string]any {
	var ae *memory.AdmissionError
	var consent *runtime.NotInstalledError
	var launch *runtime.LaunchError
	switch {
	case errors.As(err, &ae):
		return map[string]any{"code": "insufficient_memory", "message": MemoryMessage(ae)}
	case errors.As(err, &consent):
		return map[string]any{"code": "model_install_consent_required", "message": consent.Error()}
	case errors.As(err, &launch) && launch.Code == "model_hardware_ineligible":
		return map[string]any{"code": "model_hardware_ineligible", "message": launch.Message}
	case errors.As(err, &launch) && launch.Code == "experimental_model_not_allowed":
		return map[string]any{"code": "experimental_model_not_allowed", "message": launch.Message}
	}
	return map[string]any{"code": "video_generation_failed"}
}

// MemoryMessage says why the video runtime wasn't started for lack of memory, with the figures.
func MemoryMessage(e *memory.AdmissionError) string {
	accelerator := e.Reason == "accelerator-memory"
	switch {
	case e.Reason == "memory-pressure" || e.Reason == "system-memory":
		return memory.RefusalText(e, "video generation")
	case e.Reason == "measurement-unavailable":
		return "Available memory could not be measured reliably, so the video runtime was not started. Retry shortly."
	case e.PoolID == "gpu-working-set" && !e.Permanent:
		required := e.RequestedBytes
		if e.Diagnostics != nil {
			required = e.Diagnostics.Requested
		}
		resident := e.RequestedBytes - required
		return fmt.Sprintf("Not enough GPU working set to start the video runtime alongside the models already loaded: it needs %s, other loaded models use %s, and the GPU working-set limit is %s. Free memory from a loaded model, then try again.",
			memory.GiBText(required), memory.GiBText(resident), memory.GiBText(e.AvailableBytes))
	}
	pool := "memory"
	if accelerator {
		pool = "accelerator (GPU) memory"
	}
	requested, usable := int64(-1), int64(-1)
	if g := e.Diagnostics; g != nil {
		requested = g.Requested
		if g.EffectiveAvailable >= 0 && g.Reserve >= 0 {
			usable = max(0, g.EffectiveAvailable-g.Reserve)
		}
	}
	return fmt.Sprintf("Not enough free %s to start the video runtime: it needs %s but only %s is usable after the safety reserve. %s",
		pool, memory.GiBText(requested), memory.GiBText(usable), FreeMemoryHint)
}
