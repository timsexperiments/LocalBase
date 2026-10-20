package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
)

type apiError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    any     `json:"code"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError writes the OpenAI error envelope. The unread request body is
// drained first (bounded) so a rejected upload cannot stall the connection
// (#279, #305).
func writeError(w http.ResponseWriter, r *http.Request, status int, typ, code, message string, param *string) {
	if r != nil && r.Body != nil {
		io.Copy(io.Discard, io.LimitReader(r.Body, 32<<20))
	}
	writeJSON(w, status, map[string]any{"error": apiError{Message: message, Type: typ, Param: param, Code: code}})
}

func strp(s string) *string { return &s }

func routeDisabled(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, 404, "invalid_request_error", "route_disabled", "This endpoint is not exposed by this gateway.", nil)
}

func unauthorized(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, r, 401, "invalid_request_error", "invalid_api_key", "Unauthorized: Invalid or missing API key.", nil)
}

func forbidden(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, 403, "invalid_request_error", "insufficient_permissions", "Forbidden: The credential lacks the required permission.", nil)
}

func methodNotAllowed(w http.ResponseWriter, r *http.Request, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, r, 405, "invalid_request_error", "method_not_allowed", "Method not allowed.", nil)
}

func modelNotFound(w http.ResponseWriter, r *http.Request, model string) {
	writeError(w, r, 404, "invalid_request_error", "model_not_found", fmt.Sprintf("The model '%s' does not exist.", model), strp("model"))
}

func badRequest(w http.ResponseWriter, r *http.Request, message string, param *string) {
	writeError(w, r, 400, "invalid_request_error", "validation_failed", message, param)
}

func serviceUnavailable(w http.ResponseWriter, r *http.Request, service string) {
	w.Header().Set("Retry-After", "5")
	writeError(w, r, 503, "api_error", "service_unavailable", service+" service is currently restarting or unavailable. Please try again shortly.", nil)
}

// runtimeError maps runtime and admission errors to responses.
func runtimeError(w http.ResponseWriter, r *http.Request, err error, model string) {
	var ae *memory.AdmissionError
	var notInstalled *runtime.NotInstalledError
	switch {
	case errors.Is(err, runtime.ErrQueueFull):
		w.Header().Set("Retry-After", "1")
		writeError(w, r, 429, "rate_limit_error", "inference_queue_full", "The inference queue is full. Please try again shortly.", nil)
	case errors.Is(err, runtime.ErrQueueTimeout):
		w.Header().Set("Retry-After", "1")
		writeError(w, r, 429, "rate_limit_error", "inference_queue_timeout", "The inference request exceeded its queue wait deadline.", nil)
	case errors.Is(err, runtime.ErrModelNotFound):
		modelNotFound(w, r, model)
	case errors.As(err, &notInstalled):
		writeError(w, r, 409, "invalid_request_error", "model_install_consent_required", notInstalled.Error(), strp("model"))
	case errors.As(err, &ae):
		w.Header().Set("Retry-After", "5")
		msg := "Insufficient available memory to start the requested runtime. Please try again shortly."
		if ae.PoolID == "gpu-working-set" && !ae.Permanent {
			msg = fmt.Sprintf("This model needs more GPU memory than the %.1f GiB working-set limit leaves beside the models already loaded. Retry once another model is idle or unloaded.", float64(ae.AvailableBytes)/float64(memory.GiB))
		}
		writeError(w, r, 503, "api_error", "insufficient_memory", msg, nil)
	case errors.Is(err, r.Context().Err()) && r.Context().Err() != nil:
		w.WriteHeader(499)
	default:
		serviceUnavailable(w, r, "LLM")
	}
}
