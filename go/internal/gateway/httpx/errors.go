package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/timsexperiments/LocalBase/go/internal/placement"
	"net/http"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
)

type APIError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    any     `json:"code"`
}

func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// Error writes the OpenAI error envelope. A bounded part of the unread request
// body is drained first so a rejected upload cannot stall the connection (#279, #305).
func Error(w http.ResponseWriter, r *http.Request, status int, typ, code, message string, param *string) {
	drainRefused(w, r)
	JSON(w, status, map[string]any{"error": APIError{Message: message, Type: typ, Param: param, Code: code}})
}

func StrPtr(s string) *string { return &s }

func RouteDisabled(w http.ResponseWriter, r *http.Request) {
	Error(w, r, 404, "invalid_request_error", "route_disabled", "This endpoint is not exposed by this gateway.", nil)
}

func Unauthorized(w http.ResponseWriter, r *http.Request) {
	if CredentialLookupFailed(w, r) {
		return
	}
	w.Header().Set("WWW-Authenticate", "Bearer")
	Error(w, r, 401, "invalid_request_error", "invalid_api_key", "Unauthorized: Invalid or missing API key.", nil)
}

func Forbidden(w http.ResponseWriter, r *http.Request) {
	if CredentialLookupFailed(w, r) {
		return
	}
	Error(w, r, 403, "invalid_request_error", "insufficient_permissions", "Forbidden: The credential lacks the required permission.", nil)
}

func MethodNotAllowed(w http.ResponseWriter, r *http.Request, allow string) {
	w.Header().Set("Allow", allow)
	Error(w, r, 405, "invalid_request_error", "method_not_allowed", "Method not allowed.", nil)
}

func ModelNotFound(w http.ResponseWriter, r *http.Request, model string) {
	Error(w, r, 404, "invalid_request_error", "model_not_found", fmt.Sprintf("The model '%s' does not exist.", model), StrPtr("model"))
}

func BadRequest(w http.ResponseWriter, r *http.Request, message string, param *string) {
	Error(w, r, 400, "invalid_request_error", "validation_failed", message, param)
}

func ServiceUnavailable(w http.ResponseWriter, r *http.Request, service string) {
	w.Header().Set("Retry-After", "5")
	Error(w, r, 503, "api_error", "service_unavailable", service+" service is currently restarting or unavailable. Please try again shortly.", nil)
}

// RuntimeError maps runtime and admission errors to responses.
// service names the runtime in a 503 ("LLM", "STT", "TTS", "Image"), as TS does.
func RuntimeError(w http.ResponseWriter, r *http.Request, err error, model, service string) {
	if n := NoteFrom(r); n != nil && model != "" {
		n.Model = model // checked when it is counted: this came from the request
	}
	var ae *memory.AdmissionError
	var notInstalled *runtime.NotInstalledError
	var launch *runtime.LaunchError
	var started *runtime.BackendStartError
	var pe *placement.Error
	switch {
	case errors.Is(err, runtime.ErrQueueFull):
		w.Header().Set("Retry-After", "1")
		Error(w, r, 429, "rate_limit_error", "inference_queue_full", "The inference queue is full. Please try again shortly.", nil)
	case errors.Is(err, runtime.ErrQueueTimeout):
		w.Header().Set("Retry-After", "1")
		Error(w, r, 429, "rate_limit_error", "inference_queue_timeout", "The inference request exceeded its queue wait deadline.", nil)
	case errors.Is(err, runtime.ErrModelNotFound):
		ModelNotFound(w, r, model)
	case errors.Is(err, runtime.ErrMemoryEmergency):
		ServiceUnavailable(w, r, "Inference")
	case errors.Is(err, runtime.ErrUnloading):
		// An explicit unload is draining the model (DESIGN §23); retrying after
		// it loads the model again.
		w.Header().Set("Retry-After", "2")
		Error(w, r, 503, "api_error", "model_unloading", "This model is being unloaded. Retry shortly to load it again.", nil)
	case errors.As(err, &notInstalled):
		Error(w, r, 409, "invalid_request_error", "model_install_consent_required", notInstalled.Error(), StrPtr("model"))
	case errors.As(err, &launch):
		Error(w, r, 409, "invalid_request_error", launch.Code, launch.Message, StrPtr("model"))
	case errors.As(err, &started):
		// A chosen or imported backend reports its own startup error; nothing
		// else is substituted.
		// A failed start may be transient (a crash, a timeout), so clients that
		// retry on 503 get Retry-After, like every other transient failure.
		w.Header().Set("Retry-After", "5")
		Error(w, r, 503, "api_error", "backend_startup_failed", obs.BoundedText(started.Error(), 1024), nil)
	case errors.As(err, &pe):
		// CPU/GPU placement (DESIGN §20): a stored choice this device cannot
		// honour is a conflict; not fitting now may change; a backend that
		// placed tensors differently from the plan was stopped.
		switch pe.Code {
		case "model_too_large":
			Error(w, r, 422, "invalid_request_error", pe.Code, pe.Message, StrPtr("model"))
		case "placement_does_not_fit", "placement_mismatch":
			w.Header().Set("Retry-After", "5")
			Error(w, r, 503, "api_error", pe.Code, pe.Message, nil)
		default:
			Error(w, r, 409, "invalid_request_error", pe.Code, pe.Message, StrPtr("model"))
		}
	case errors.As(err, &ae):
		if ae.Permanent {
			modelTooLarge(w, r, ae)
			return
		}
		w.Header().Set("Retry-After", "5")
		msg := MemoryRefusal(ae, "this model")
		if ae.PoolID == "gpu:0" {
			msg = fmt.Sprintf("Not enough free GPU memory to load this model with its placement right now: it needs %.1f GiB on the GPU, %.1f GiB is free and %.1f GiB is kept free. Retry shortly, or choose a placement that keeps more in system memory.",
				float64(ae.RequestedBytes)/float64(memory.GiB), float64(ae.AvailableBytes)/float64(memory.GiB), float64(ae.ReserveBytes)/float64(memory.GiB))
		}
		if ae.PoolID == "gpu-working-set" && !ae.Permanent {
			msg = fmt.Sprintf("This model needs more GPU memory than the %.1f GiB working-set limit leaves beside the models already loaded. Retry once another model is idle, or free memory from one.", float64(ae.AvailableBytes)/float64(memory.GiB))
		}
		Error(w, r, 503, "api_error", "insufficient_memory", msg, nil)
	case errors.Is(err, r.Context().Err()) && r.Context().Err() != nil:
		w.WriteHeader(499)
	default:
		// Transient (a crash, a start timeout): clients retry after Retry-After.
		ServiceUnavailable(w, r, service)
	}
}

// modelTooLarge answers a model that can never be admitted on this host, even
// with everything else stopped: a 422 with no Retry-After (TS resourceUnavailable),
// because retrying can never work.
func modelTooLarge(w http.ResponseWriter, r *http.Request, ae *memory.AdmissionError) {
	gib := func(b int64) string { return fmt.Sprintf("%.1f GiB", float64(b)/float64(memory.GiB)) }
	what, where := "memory", "models on this host"
	if ae.PoolID == "gpu:0" { // Go only: placement's GPU pool (DESIGN §20)
		what, where = "GPU memory", "models on this GPU"
	}
	msg := fmt.Sprintf("This model requires about %s of %s, but only %s is available for %s (%s total, after the safety reserve). It cannot be loaded here even when nothing else is running. Use a smaller quantization or a smaller model, a smaller context size, or keep less for the computer: set memory.systemReserve (percent and minimumGb) with local-base configure --config.",
		gib(ae.RequestedBytes), what, gib(ae.AvailableBytes), where, gib(ae.CapacityBytes))
	Error(w, r, 422, "invalid_request_error", "model_too_large", msg, StrPtr("model"))
}

// MemoryRefusal says why a runtime wasn't started, what has the memory and what to do. The figures
// are the host's reserve, what is free and what LocalBase has reserved, never which models. Anything
// else keeps the general sentence.
func MemoryRefusal(e *memory.AdmissionError, subject string) string {
	var free string
	requested := e.RequestedBytes
	if g := e.Diagnostics; g != nil {
		requested = g.Requested
		if g.EffectiveAvailable >= 0 && g.Reserve >= 0 {
			free = fmt.Sprintf(", and %s is free after keeping %s for your computer", GiBText(max(0, g.EffectiveAvailable-g.Reserve)), GiBText(g.Reserve))
		}
	}
	reserved := "LocalBase's loaded models have " + GiBText(e.ReservedBytes) + " reserved"
	action := "Free memory from a loaded model or close other apps, then try again."
	if e.ReservedBytes == 0 {
		reserved = ""
		action = "LocalBase has nothing loaded, so other apps are using this memory. Close other apps, then try again."
	}
	switch e.Reason {
	case "memory-pressure":
		switch {
		case requested > 0 && reserved != "":
			action = "It needs " + GiBText(requested) + ", and " + reserved + ". " + action
		case requested > 0:
			action = "It needs " + GiBText(requested) + ". " + action
		case reserved != "":
			action = reserved + ". " + action
		}
		return fmt.Sprintf("This computer is under memory pressure, so %s wasn't started. %s", subject, action)
	case "system-memory":
		if reserved != "" {
			action = reserved + ". " + action
		}
		if requested <= 0 { // Not known: the job's failure didn't carry it.
			return fmt.Sprintf("Not enough free memory to start %s%s. %s", subject, free, action)
		}
		return fmt.Sprintf("Not enough free memory to start %s: it needs %s%s. %s", subject, GiBText(requested), free, action)
	}
	return "Insufficient available memory to start the requested runtime. Please try again shortly."
}
