package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
)

// GET /_localbase/host and GET /_localbase/models/{id}/admission (DESIGN §26): what
// the machine is doing, truthfully and cheaply. An unknown figure is null, never 0.
// Reads share one sample a second, so polling never spawns a process per call.

// computerNameKey holds the owner's name for this computer. It stays out of logs
// and diagnostics.
const computerNameKey = "host.name"

// uiPollPath is what every visible page polls every few seconds, so it is not
// traced at all. Pages are all one mux pattern, so this goes by path.
const uiPollPath = "/app/strip"

// quietRoute: successful probes and page polls are not request-logged.
func quietRoute(label, path string) bool {
	return label == "/health" || label == "/health/ready" || path == uiPollPath
}

type memoryFigure struct {
	Bytes  *int64 `json:"bytes"`
	Source string `json:"source"`
}

type runtimeFigure struct {
	Runtime string  `json:"runtime"`
	ModelID *string `json:"modelId"`
	Bytes   int64   `json:"bytes"`
	// Source is measured, or planned when there is no reading.
	Source        string `json:"source"`
	PlannedBytes  int64  `json:"plannedBytes"`
	MeasuredBytes *int64 `json:"measuredBytes"`
}

type activityFigure struct {
	Configured bool `json:"configured"`
	InFlight   int  `json:"inFlight"`
	Queued     int  `json:"queued"`
}

func known(v int64) *int64 {
	if v < 0 {
		return nil
	}
	return &v
}

// computerName is the owner's name, else this machine's, without .local.
func (s *Server) computerName(ctx context.Context) (string, string) {
	if v, ok, err := s.Store.Setting(ctx, computerNameKey); err == nil && ok && v != "" {
		return v, "setting"
	}
	h, err := os.Hostname()
	if err != nil {
		return "", "unavailable"
	}
	return strings.TrimSuffix(h, ".local"), "system"
}

func (s *Server) runtimeModel(name string) *string {
	id := ""
	switch name {
	case "llm":
		if s.LLM != nil {
			id, _, _ = s.LLM.Running()
		}
	default:
		if svc := s.service(name); svc != nil {
			id = svc.Running()
		}
	}
	if id == "" {
		return nil
	}
	return &id
}

func (s *Server) service(name string) *runtime.Service {
	switch name {
	case "stt":
		return s.STT
	case "tts":
		return s.TTS
	case "image":
		return s.Image
	case "video":
		return s.Video
	}
	return nil
}

func (s *Server) hostMemory(ctx context.Context) map[string]any {
	topo := s.Memory.Topology()
	snap := s.Memory.Snapshot(ctx)
	total, available := int64(-1), snap.Available
	if topo.SystemCapacity > 0 {
		total = topo.SystemCapacity
	}
	figure := func(v int64) memoryFigure {
		if v < 0 {
			return memoryFigure{Source: "unavailable"}
		}
		return memoryFigure{Bytes: &v, Source: "system"}
	}
	runtimes := []runtimeFigure{}
	var sum int64
	measured, planned := 0, 0
	for _, m := range s.Memory.Runtimes(ctx) {
		f := runtimeFigure{Runtime: m.Runtime, ModelID: s.runtimeModel(m.Runtime), Bytes: m.PlannedBytes, Source: "planned",
			PlannedBytes: m.PlannedBytes, MeasuredBytes: known(m.MeasuredBytes)}
		if m.MeasuredBytes >= 0 {
			f.Bytes, f.Source = m.MeasuredBytes, "measured"
			measured++
		} else {
			planned++
		}
		sum += f.Bytes
		runtimes = append(runtimes, f)
	}
	source := "none"
	switch {
	case measured > 0 && planned > 0:
		source = "mixed"
	case measured > 0:
		source = "measured"
	case planned > 0:
		source = "planned"
	}
	other := memoryFigure{Source: "unavailable"}
	if total >= 0 && available >= 0 {
		v := max(0, total-available-sum)
		other = memoryFigure{Bytes: &v, Source: "derived"}
	}
	var pressure *string
	if available >= 0 {
		state := s.Memory.PressureState()
		pressure = &state
	}
	return map[string]any{"total": figure(total), "available": figure(available), "pressure": pressure,
		"localBase": map[string]any{"bytes": sum, "source": source, "runtimes": runtimes}, "other": other}
}

func (s *Server) hostActivity() map[string]any {
	out := map[string]any{}
	busy := false
	add := func(name string, configured bool, inFlight, queued int) {
		out[name] = activityFigure{configured, inFlight, queued}
		busy = busy || inFlight > 0 || queued > 0
	}
	if s.LLM != nil {
		i, q := s.LLM.Activity()
		add("llm", true, i, q)
	} else {
		add("llm", false, 0, 0)
	}
	for _, name := range []string{"stt", "tts", "image", "video"} {
		svc := s.service(name)
		if svc == nil {
			add(name, false, 0, 0)
			continue
		}
		i, q := svc.Activity()
		if name == "video" && s.VideoJobs != nil {
			i = max(i, s.VideoJobs.ActiveJobs())
		}
		add(name, true, i, q)
	}
	runs := 0
	if s.runs != nil {
		runs = s.runs.activeCount()
	}
	out["runs"] = map[string]int{"active": runs}
	out["busy"] = busy || runs > 0
	return out
}

// hostAPI serves GET /_localbase/host (models:read).
func (s *Server) hostAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r, "GET")
		return
	}
	name, source := s.computerName(r.Context())
	var named *string
	if name != "" {
		named = &name
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"host": map[string]any{
		"name": named, "nameSource": source, "memory": s.hostMemory(r.Context()), "activity": s.hostActivity(),
	}, "capturedAt": time.Now().UnixMilli()})
}

// computerNameAPI serves PUT /_localbase/computer-name (models:manage). An empty
// name goes back to the machine's own.
func (s *Server) computerNameAPI(w http.ResponseWriter, r *http.Request) {
	p := s.principal(r)
	if !p.Authenticated() {
		unauthorized(w, r)
		return
	}
	if r.Method != http.MethodPut {
		methodNotAllowed(w, r, "PUT")
		return
	}
	if !p.Can("models:manage") {
		forbidden(w, r)
		return
	}
	var in struct {
		Name *string `json:"name"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1025))
	if err != nil || len(body) > 1024 || json.Unmarshal(body, &in) != nil || in.Name == nil {
		badRequest(w, r, "Provide a name.", nil)
		return
	}
	name := strings.TrimSpace(*in.Name)
	if len([]rune(name)) > 64 || strings.ContainsFunc(name, unicode.IsControl) {
		badRequest(w, r, "A computer name is at most 64 characters, with no control characters.", nil)
		return
	}
	if err := s.Store.SetSetting(r.Context(), computerNameKey, name); err != nil {
		serviceUnavailable(w, r, "Settings")
		return
	}
	got, source := s.computerName(r.Context())
	writeJSON(w, 200, map[string]any{"name": got, "nameSource": source})
}

// refusalWriter captures what runtimeError answers, so a preview shows the real
// refusal's status, code and message.
type refusalWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *refusalWriter) Header() http.Header         { return w.header }
func (w *refusalWriter) WriteHeader(status int)      { w.status = status }
func (w *refusalWriter) Write(b []byte) (int, error) { return w.body.Write(b) }

func (s *Server) refusalFor(r *http.Request, err error, id, service string) map[string]any {
	var ae *memory.AdmissionError
	permanent := errors.As(err, &ae) && ae.Permanent
	if service == "video" {
		// A video request is accepted and its job then fails with this code.
		f := videoFailureError(err)
		return map[string]any{"status": nil, "jobFailure": true, "code": f["code"], "message": f["message"], "permanent": permanent, "retryAfter": nil}
	}
	rec := &refusalWriter{header: http.Header{}}
	runtimeError(rec, r.WithContext(r.Context()), err, id, service)
	var parsed struct {
		Error struct {
			Code    any    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.body.Bytes(), &parsed) // a body that is not JSON has no message to show
	var retry any
	if v := rec.header.Get("Retry-After"); v != "" {
		retry = v
	}
	return map[string]any{"status": rec.status, "jobFailure": false, "code": parsed.Error.Code, "message": parsed.Error.Message,
		"permanent": permanent, "retryAfter": retry}
}

// admissionAPI serves GET /_localbase/models/{id}/admission: what a request for
// this model would meet now. It runs the real admission's evaluate and maps the
// outcome through the real refusal, and starts, stops and installs nothing.
func (s *Server) admissionAPI(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r, "GET")
		return
	}
	var (
		preview memory.AdmissionPreview
		demand  memory.Demand
		err     error
		name    = "llm"
		service = "LLM"
	)
	// ?assumeInstalled=1 answers the memory question alone, for a model not yet
	// on disk. The real request would first need the install.
	assume := r.URL.Query().Get("assumeInstalled") == "1"
	if spec := s.Catalog.ByID(id); spec != nil {
		name = spec.Kind
	} else if _, _, err := s.LLM.Resolve(id); err != nil {
		modelNotFound(w, r, id) // neither in the catalog nor imported
		return
	}
	switch name {
	case "llm":
		preview, demand, err = s.LLM.PreviewAdmission(r.Context(), id, assume)
	default:
		svc := s.service(name)
		if svc == nil {
			notConfigured(w, r, strings.ToUpper(name))
			return
		}
		service = map[string]string{"stt": "STT", "tts": "TTS", "image": "Image", "video": "video"}[name]
		preview, demand, err = svc.PreviewAdmission(r.Context(), id, assume)
	}
	if errors.Is(err, runtime.ErrModelNotFound) {
		modelNotFound(w, r, id)
		return
	}
	if err == nil {
		err = preview.Err
	}
	body := map[string]any{"modelId": id, "runtime": name, "admitted": err == nil, "demandSource": demand.Confidence, "refusal": nil,
		"requestedBytes": nil, "reserveBytes": nil, "availableBytes": nil, "pool": nil}
	var ae *memory.AdmissionError
	switch {
	case preview.RequestedBytes > 0 || preview.ReserveBytes > 0:
		body["requestedBytes"], body["reserveBytes"] = preview.RequestedBytes, preview.ReserveBytes
		body["availableBytes"] = known(preview.AvailableBytes)
		body["pool"] = preview.PoolID
	case errors.As(err, &ae):
		body["requestedBytes"], body["reserveBytes"] = ae.RequestedBytes, ae.ReserveBytes
	}
	if err != nil {
		body["refusal"] = s.refusalFor(r, err, id, service)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, body)
}
