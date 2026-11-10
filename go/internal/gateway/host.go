package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/upgrade"
	"github.com/timsexperiments/LocalBase/go/internal/usage"
	"github.com/timsexperiments/LocalBase/go/internal/video"
	"github.com/timsexperiments/LocalBase/go/internal/voices"
)

// GET /_localbase/host and GET /_localbase/models/{id}/admission (DESIGN §26): what
// the machine is doing, truthfully and cheaply. An unknown figure is null, never 0.
// Reads share one sample a second, so polling never spawns a process per call.

// Handler serves the host, computer name, usage, update and voices routes for one gateway.
type Handler struct {
	opts Options
}

// Options are what Handler needs from the gateway. The funcs that read a value are read when
// called: tests set those on the gateway after New.
type Options struct {
	Store     *store.Store
	Catalog   *catalog.Catalog
	Memory    *memory.Controller
	Usage     *usage.Recorder
	Updates   *upgrade.Checker
	VideoJobs *video.Manager
	Runtimes  func() runtime.Set
	Voices    func() *voices.Store
	Log       func() *slog.Logger
	NoAuth    func() bool
	// Caller is who a request is from, LocalCLI whether it is this machine's CLI, and
	// UsagePrincipal who to count it under.
	Caller         func(*http.Request) *auth.Principal
	LocalCLI       func(*http.Request) bool
	UsagePrincipal func(*auth.Principal) (kind, id string, ok bool)
	// ActiveRuns is how many detached runs have not finished.
	ActiveRuns func() int
	// Unmatched answers a path no route has.
	Unmatched http.HandlerFunc
}

// NewHandler returns the host and records routes for o.
func NewHandler(o Options) *Handler { return &Handler{opts: o} }

// computerNameKey holds the owner's name for this computer. It stays out of logs
// and diagnostics.
const computerNameKey = "host.name"

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
func (h *Handler) computerName(ctx context.Context) (string, string) {
	if v, ok, err := h.opts.Store.Setting(ctx, computerNameKey); err == nil && ok && v != "" {
		return v, "setting"
	}
	name, err := os.Hostname()
	if err != nil {
		return "", "unavailable"
	}
	return strings.TrimSuffix(name, ".local"), "system"
}

func (h *Handler) runtimeModel(name string) *string {
	id := ""
	switch name {
	case "llm":
		if h.opts.Runtimes().LLM != nil {
			id, _, _ = h.opts.Runtimes().LLM.Running()
		}
	default:
		if svc := h.service(name); svc != nil {
			id = svc.Running()
		}
	}
	if id == "" {
		return nil
	}
	return &id
}

func (h *Handler) service(name string) *runtime.Service {
	switch name {
	case "stt":
		return h.opts.Runtimes().STT
	case "tts":
		return h.opts.Runtimes().TTS
	case "image":
		return h.opts.Runtimes().Image
	case "video":
		return h.opts.Runtimes().Video
	}
	return nil
}

func (h *Handler) hostMemory(ctx context.Context) map[string]any {
	topo := h.opts.Memory.Topology()
	snap := h.opts.Memory.Snapshot(ctx)
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
	for _, m := range h.opts.Memory.Runtimes(ctx) {
		f := runtimeFigure{Runtime: m.Runtime, ModelID: h.runtimeModel(m.Runtime), Bytes: m.PlannedBytes, Source: "planned",
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
		state := h.opts.Memory.PressureState()
		pressure = &state
	}
	out := map[string]any{"total": figure(total), "available": figure(available), "pressure": pressure,
		"localBase": map[string]any{"bytes": sum, "source": source, "runtimes": runtimes}, "other": other}
	if available >= 0 && h.opts.Memory.Settling() {
		out["settling"] = true // absent otherwise
	}
	return out
}

func (h *Handler) hostActivity() map[string]any {
	out := map[string]any{}
	busy := false
	add := func(name string, configured bool, inFlight, queued int) {
		out[name] = activityFigure{configured, inFlight, queued}
		busy = busy || inFlight > 0 || queued > 0
	}
	if h.opts.Runtimes().LLM != nil {
		i, q := h.opts.Runtimes().LLM.Activity()
		add("llm", true, i, q)
	} else {
		add("llm", false, 0, 0)
	}
	for _, name := range []string{"stt", "tts", "image", "video"} {
		svc := h.service(name)
		if svc == nil {
			add(name, false, 0, 0)
			continue
		}
		i, q := svc.Activity()
		if name == "video" && h.opts.VideoJobs != nil {
			i = max(i, h.opts.VideoJobs.ActiveJobs())
		}
		add(name, true, i, q)
	}
	runs := 0
	if h.opts.ActiveRuns != nil {
		runs = h.opts.ActiveRuns()
	}
	out["runs"] = map[string]int{"active": runs}
	out["busy"] = busy || runs > 0
	return out
}

// hostAPI serves GET /_localbase/host (models:read).
func (h *Handler) HostAPI(w http.ResponseWriter, r *http.Request) {
	name, source := h.computerName(r.Context())
	var named *string
	if name != "" {
		named = &name
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, 200, map[string]any{"host": map[string]any{
		"name": named, "nameSource": source, "memory": h.hostMemory(r.Context()), "activity": h.hostActivity(),
	}, "capturedAt": time.Now().UnixMilli()})
}

// computerNameAPI serves PUT /_localbase/computer-name (models:manage). An empty
// name goes back to the machine's own.
func (h *Handler) ComputerNameAPI(w http.ResponseWriter, r *http.Request) {
	p := h.opts.Caller(r)
	if !p.Authenticated() {
		httpx.Unauthorized(w, r)
		return
	}
	if r.Method != http.MethodPut {
		// Checked here, not by the route table's gate: this handler authenticates the caller first, and 401 comes before 405.
		httpx.MethodNotAllowed(w, r, "PUT")
		return
	}
	if !p.Can("models:manage") {
		httpx.Forbidden(w, r)
		return
	}
	var in struct {
		Name *string `json:"name"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1025))
	if err != nil || len(body) > 1024 || json.Unmarshal(body, &in) != nil || in.Name == nil {
		httpx.BadRequest(w, r, "Provide a name.", nil)
		return
	}
	name := strings.TrimSpace(*in.Name)
	if len([]rune(name)) > 64 || strings.ContainsFunc(name, unicode.IsControl) {
		httpx.BadRequest(w, r, "A computer name is at most 64 characters, with no control characters.", nil)
		return
	}
	if err := h.opts.Store.SetSetting(r.Context(), computerNameKey, name); err != nil {
		httpx.ServiceUnavailable(w, r, "Settings")
		return
	}
	got, source := h.computerName(r.Context())
	httpx.JSON(w, 200, map[string]any{"name": got, "nameSource": source})
}

// refusalWriter captures what RuntimeError answers, so a preview shows the real
// refusal's status, code and message.
type refusalWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *refusalWriter) Header() http.Header         { return w.header }
func (w *refusalWriter) WriteHeader(status int)      { w.status = status }
func (w *refusalWriter) Write(b []byte) (int, error) { return w.body.Write(b) }

func (h *Handler) refusalFor(r *http.Request, err error, id, service string) map[string]any {
	var ae *memory.AdmissionError
	permanent := errors.As(err, &ae) && ae.Permanent
	if service == "video" {
		// A video request is accepted and its job then fails with this code.
		f := videoFailureError(err)
		return map[string]any{"status": nil, "jobFailure": true, "code": f["code"], "message": f["message"], "permanent": permanent, "retryAfter": nil}
	}
	rec := &refusalWriter{header: http.Header{}}
	httpx.RuntimeError(rec, r.WithContext(r.Context()), err, id, service)
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
func (h *Handler) AdmissionAPI(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		// Checked here, not by the route table's gate: the method allowed depends on the path below the route.
		httpx.MethodNotAllowed(w, r, "GET")
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
	if spec := h.opts.Catalog.ByID(id); spec != nil {
		name = spec.Kind
	} else if _, _, err := h.opts.Runtimes().LLM.Resolve(id); err != nil {
		httpx.ModelNotFound(w, r, id) // neither in the catalog nor imported
		return
	}
	switch name {
	case "llm":
		preview, demand, err = h.opts.Runtimes().LLM.PreviewAdmission(r.Context(), id, assume)
	default:
		svc := h.service(name)
		if svc == nil {
			notConfigured(w, r, strings.ToUpper(name))
			return
		}
		service = map[string]string{"stt": "STT", "tts": "TTS", "image": "Image", "video": "video"}[name]
		preview, demand, err = svc.PreviewAdmission(r.Context(), id, assume)
	}
	if errors.Is(err, runtime.ErrModelNotFound) {
		httpx.ModelNotFound(w, r, id)
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
		body["refusal"] = h.refusalFor(r, err, id, service)
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, 200, body)
}
