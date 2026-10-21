package gateway

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
)

// TestRuntimeErrorRetrySemantics pins the status contract clients rely on:
// transient failures are 503 or 429 with Retry-After; failures that need
// someone to act are 4xx without it.
func TestRuntimeErrorRetrySemantics(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
		retry  string
	}{
		{"start crashed or timed out", fmt.Errorf("%w: sd-server exited during startup", runtime.ErrStartupFailed), 503, "service_unavailable", "5"},
		{"runtime unavailable", runtime.ErrUnavailable, 503, "service_unavailable", "5"},
		{"chosen backend failed to start", &runtime.BackendStartError{BackendID: "x", Cause: errors.New("did not become healthy")}, 503, "backend_startup_failed", "5"},
		{"memory pressure", &memory.AdmissionError{Reason: "insufficient"}, 503, "insufficient_memory", "5"},
		{"queue full", runtime.ErrQueueFull, 429, "inference_queue_full", "1"},
		{"queue timeout", runtime.ErrQueueTimeout, 429, "inference_queue_timeout", "1"},
		{"install consent", &runtime.NotInstalledError{ModelID: "m"}, 409, "model_install_consent_required", ""},
		{"no backend selected", &runtime.LaunchError{Code: "backend_not_selected", Message: "Choose a backend."}, 409, "backend_not_selected", ""},
		{"assets missing", &runtime.LaunchError{Code: "model_assets_missing", Message: "Files are missing."}, 409, "model_assets_missing", ""},
		{"unknown model", runtime.ErrModelNotFound, 404, "model_not_found", ""},
		{"memory emergency", runtime.ErrMemoryEmergency, 503, "service_unavailable", "5"},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		runtimeError(w, httptest.NewRequest("POST", "/v1/images/generations", nil), c.err, "m", "Image")
		if w.Code != c.status || !strings.Contains(w.Body.String(), `"code":"`+c.code+`"`) || w.Header().Get("Retry-After") != c.retry {
			t.Errorf("%s: %d Retry-After=%q %s", c.name, w.Code, w.Header().Get("Retry-After"), w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	runtimeError(w, httptest.NewRequest("POST", "/v1/audio/speech", nil), runtime.ErrStartupFailed, "m", "TTS")
	if !strings.Contains(w.Body.String(), "TTS service is currently restarting") {
		t.Errorf("service name: %s", w.Body.String())
	}
}

// TestMemoryEmergencyResponse is TS's answer to work queued when memory turns
// critical (InferenceQueueUnavailableError): the Inference service is
// unavailable, whichever modality was asked.
func TestMemoryEmergencyResponse(t *testing.T) {
	w := httptest.NewRecorder()
	runtimeError(w, httptest.NewRequest("POST", "/v1/audio/transcriptions", nil), runtime.ErrMemoryEmergency, "m", "STT")
	if w.Code != 503 || !strings.Contains(w.Body.String(), "Inference service is currently restarting or unavailable. Please try again shortly.") {
		t.Errorf("%d %s", w.Code, w.Body.String())
	}
}
