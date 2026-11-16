package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/placement"
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
		{"the device placed it differently", &placement.Error{Code: "placement_mismatch", Message: "It differs."}, 503, "placement_mismatch", "5"},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		httpx.RuntimeError(w, httptest.NewRequest("POST", "/v1/images/generations", nil), c.err, "m", "Image")
		if w.Code != c.status || !strings.Contains(w.Body.String(), `"code":"`+c.code+`"`) || w.Header().Get("Retry-After") != c.retry {
			t.Errorf("%s: %d Retry-After=%q %s", c.name, w.Code, w.Header().Get("Retry-After"), w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	httpx.RuntimeError(w, httptest.NewRequest("POST", "/v1/audio/speech", nil), runtime.ErrStartupFailed, "m", "TTS")
	if !strings.Contains(w.Body.String(), "TTS service is currently restarting") {
		t.Errorf("service name: %s", w.Body.String())
	}
}

// TestMemoryEmergencyResponse is TS's answer to work queued when memory turns
// critical (InferenceQueueUnavailableError): the Inference service is
// unavailable, whichever modality was asked.
func TestMemoryEmergencyResponse(t *testing.T) {
	w := httptest.NewRecorder()
	httpx.RuntimeError(w, httptest.NewRequest("POST", "/v1/audio/transcriptions", nil), runtime.ErrMemoryEmergency, "m", "STT")
	if w.Code != 503 || !strings.Contains(w.Body.String(), "Inference service is currently restarting or unavailable. Please try again shortly.") {
		t.Errorf("%d %s", w.Code, w.Body.String())
	}
}

// A model that can never fit is a permanent 422 with no Retry-After, with TS's
// exact words: telling someone to retry would never work. This text is what TS
// answered on a 7 GiB CI runner, where the unified GPU working set is the limit.
func TestModelTooLarge(t *testing.T) {
	const gib = int64(1) << 30
	tenth := func(n int64) int64 { return n * gib / 10 }
	for name, c := range map[string]struct {
		err  *memory.AdmissionError
		want string
	}{
		"GPU working set": {&memory.AdmissionError{Reason: "accelerator-memory", PoolID: "gpu-working-set", Permanent: true,
			RequestedBytes: tenth(145), AvailableBytes: tenth(47), CapacityBytes: tenth(47)},
			"This model requires about 14.5 GiB of memory, but only 4.7 GiB is available for models on this host (4.7 GiB total, after the safety reserve). It cannot be loaded here even when nothing else is running. Use a smaller quantization or a smaller model, a smaller context size, or keep less for the computer: set memory.systemReserve (percent and minimumGb) with local-base configure --config."},
		"system memory": {&memory.AdmissionError{Reason: "system-memory", PoolID: "system", Permanent: true,
			RequestedBytes: 20 * gib, AvailableBytes: 8 * gib, CapacityBytes: 16 * gib},
			"This model requires about 20.0 GiB of memory, but only 8.0 GiB is available for models on this host (16.0 GiB total, after the safety reserve). It cannot be loaded here even when nothing else is running. Use a smaller quantization or a smaller model, a smaller context size, or keep less for the computer: set memory.systemReserve (percent and minimumGb) with local-base configure --config."},
	} {
		w := httptest.NewRecorder()
		httpx.RuntimeError(w, httptest.NewRequest("POST", "/v1/images/generations", nil), c.err, "m", "Image")
		var body struct {
			Error struct {
				Message, Type, Code string
				Param               *string
			}
		}
		json.Unmarshal(w.Body.Bytes(), &body)
		e := body.Error
		if w.Code != 422 || e.Code != "model_too_large" || e.Type != "invalid_request_error" || e.Param == nil || *e.Param != "model" || e.Message != c.want {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
		if got := w.Header().Get("Retry-After"); got != "" {
			t.Errorf("%s: Retry-After %q on something that can never succeed", name, got)
		}
	}
	// The GPU pool says GPU memory. Also permanent.
	w := httptest.NewRecorder()
	httpx.RuntimeError(w, httptest.NewRequest("POST", "/v1/chat/completions", nil), &memory.AdmissionError{Reason: "gpu-memory", PoolID: "gpu:0", Permanent: true,
		RequestedBytes: 13 * gib, AvailableBytes: 11 * gib, CapacityBytes: 12 * gib}, "m", "LLM")
	if w.Code != 422 || w.Header().Get("Retry-After") != "" || !strings.Contains(w.Body.String(), "model_too_large") || !strings.Contains(w.Body.String(), "GPU memory") {
		t.Errorf("gpu:0: %d %s", w.Code, w.Body)
	}
	// A shortage that can pass stays a retryable 503.
	w = httptest.NewRecorder()
	httpx.RuntimeError(w, httptest.NewRequest("POST", "/v1/chat/completions", nil), &memory.AdmissionError{Reason: "system-memory", PoolID: "system"}, "m", "LLM")
	if w.Code != 503 || w.Header().Get("Retry-After") != "5" {
		t.Errorf("transient: %d %q", w.Code, w.Header().Get("Retry-After"))
	}
}

// smallHost is a unified host too small for the chat model.
type smallHost struct{}

func (smallHost) Topology() memory.Topology {
	return memory.Topology{Unified: true, SystemCapacity: 16 * memory.GiB, GPUWorkingSetBytes: 1 * memory.GiB}
}
func (smallHost) Snapshot(context.Context) memory.Snapshot {
	return memory.Snapshot{Available: 15 * memory.GiB, Pressure: "normal"}
}

// Through a real gateway and the real admission path, and through a detached run.
func TestModelTooLargeEndToEnd(t *testing.T) {
	ts, llm, _ := newTestGateway(t, smallHost{})
	r := post(t, ts, "/v1/chat/completions", gatewaytest.ChatBody("hi"))
	if r.status != 422 || r.code() != "model_too_large" || r.header.Get("Retry-After") != "" {
		t.Errorf("/v1: %d %q %s", r.status, r.header.Get("Retry-After"), r.body)
	}
	if id, _, _ := llm.Running(); id != "" {
		t.Errorf("%s started", id)
	}
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	m := c.WaitStatus(c.Start("hi"), "failed")
	if e, _ := m["error"].(map[string]any); e["code"] != "model_too_large" {
		t.Errorf("run: %v", m)
	}
}

// reserveHost has no GPU limit, so the reserve alone decides.
type reserveHost struct{ total, avail int64 }

func (h reserveHost) Topology() memory.Topology {
	return memory.Topology{Unified: true, SystemCapacity: h.total}
}
func (h reserveHost) Snapshot(context.Context) memory.Snapshot {
	return memory.Snapshot{Available: h.avail, Pressure: "normal"}
}

// A reserve that makes a model impossible is a 422 with no retry, however much is
// free; memory that is only low right now is a 503 with Retry-After.
func TestReserveRefusalsAreDistinct(t *testing.T) {
	ts, _, _ := newTestGateway(t, reserveHost{8 * memory.GiB, 7 * memory.GiB})
	r := post(t, ts, "/v1/chat/completions", gatewaytest.ChatBody("hi"))
	if r.status != 422 || r.code() != "model_too_large" || r.header.Get("Retry-After") != "" || !strings.Contains(r.body, "after the safety reserve") {
		t.Errorf("impossible: %d %q Retry-After=%q %s", r.status, r.code(), r.header.Get("Retry-After"), r.body)
	}
	ts, _, _ = newTestGateway(t, reserveHost{64 * memory.GiB, 10 * memory.GiB})
	r = post(t, ts, "/v1/chat/completions", gatewaytest.ChatBody("hi"))
	if r.status != 503 || r.code() != "insufficient_memory" || r.header.Get("Retry-After") == "" {
		t.Errorf("low right now: %d %q Retry-After=%q %s", r.status, r.code(), r.header.Get("Retry-After"), r.body)
	}
}
