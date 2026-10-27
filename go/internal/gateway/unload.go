package gateway

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/runtime"
)

// unloadWait bounds how long an unload waits for in-flight requests.
var unloadWait = 30 * time.Second

// unloadAPI is POST /_localbase/models/{id}/unload (models:manage, DESIGN
// §23): drain the loaded model like an eviction, then stop it and release
// its reservation. ?wait=false refuses at once while requests (detached
// runs included) are in flight.
func (s *Server) unloadAPI(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, "POST")
		return
	}
	if p := s.principal(r); !s.NoAuth && !p.Can("models:manage") {
		forbidden(w, r)
		return
	}
	wait := unloadWait
	switch r.URL.Query().Get("wait") {
	case "", "true":
	case "false":
		wait = 0
	default:
		badRequest(w, r, "wait must be true or false.", strp("wait"))
		return
	}
	name, unload := s.loadedRuntime(id)
	if unload == nil {
		if s.Catalog.ByID(id) == nil && s.importedTask(id) == "" {
			modelNotFound(w, r, id)
			return
		}
		writeError(w, r, 404, "invalid_request_error", "model_not_loaded", "Model '"+id+"' is not loaded.", strp("model"))
		return
	}
	started := time.Now()
	freed, err := unload(r, id, wait)
	var busy *runtime.BusyError
	switch {
	case errors.Is(err, runtime.ErrNotLoaded):
		writeError(w, r, 404, "invalid_request_error", "model_not_loaded", "Model '"+id+"' is not loaded.", strp("model"))
	case errors.As(err, &busy):
		msg := "Model '" + id + "' is busy: " + strconv.Itoa(busy.InFlight) + " request(s) in flight."
		if wait > 0 {
			msg += " It did not finish within " + strconv.Itoa(int(wait.Seconds())) + " s; try again, or stop its generations first."
		} else {
			msg += " Try again when they finish, or unload with waiting."
		}
		writeJSON(w, 409, map[string]any{"error": map[string]any{"message": msg, "type": "invalid_request_error",
			"param": nil, "code": "model_busy", "in_flight": busy.InFlight}})
	case err != nil:
		serviceUnavailable(w, r, "Model unload")
	default:
		writeJSON(w, 200, map[string]any{"id": id, "object": "localbase.model.unload", "unloaded": true, "runtime": name,
			"memoryFreedBytes": freed, "drainedMs": time.Since(started).Milliseconds()})
	}
}

// loadedRuntime is the runtime that has id loaded now, and its unload.
func (s *Server) loadedRuntime(id string) (string, func(*http.Request, string, time.Duration) (int64, error)) {
	if s.LLM != nil {
		if running, _, _ := s.LLM.Running(); running == id {
			return "llm", func(r *http.Request, id string, wait time.Duration) (int64, error) {
				return s.LLM.Unload(r.Context(), id, wait)
			}
		}
	}
	for _, svc := range []struct {
		name string
		s    *runtime.Service
	}{{"stt", s.STT}, {"tts", s.TTS}, {"image", s.Image}, {"video", s.Video}} {
		if svc.s != nil && svc.s.Running() == id {
			return svc.name, func(r *http.Request, id string, wait time.Duration) (int64, error) {
				return svc.s.Unload(r.Context(), id, wait)
			}
		}
	}
	return "", nil
}
