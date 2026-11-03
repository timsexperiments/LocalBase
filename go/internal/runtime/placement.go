package runtime

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os/exec"
	goruntime "runtime"
	"sync"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/store"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/placement"
)

// place resolves the CPU/GPU placement for this load (DESIGN §20). The plan
// is kept with the load: it is re-resolved only when the model loads again.
func (m *LLM) place(modelID string, plan *LLMPlan, t *Target) error {
	if m.d.Placement == nil {
		return nil
	}
	return m.placeWith(modelID, plan, t, m.d.Placement.Request(modelID))
}

// placeWith resolves req for this load and keeps it on the plan.
func (m *LLM) placeWith(modelID string, plan *LLMPlan, t *Target, req placement.Request) error {
	if m.d.Placement == nil || plan.Embedding {
		return nil
	}
	layers := 0
	if plan.Geometry != nil {
		layers = plan.Geometry.BlockCount
	}
	if layers == 0 {
		return nil // no geometry: today's launch, nothing planned
	}
	model, err := placement.LoadModel(plan.ModelPath, layers)
	if err != nil {
		return nil // an unreadable tensor table keeps today's launch
	}
	dev := m.d.Placement.Device(t)
	model.HostExtraBytes = memory.HostExtraBytes(memory.ResolveShape(planSize(m, modelID, t), planQuant(m, modelID, t), plan.ArtifactBytes, plan.Geometry), plan.Launch)
	kv := memory.KVCacheBytes(plan.Geometry, plan.Launch.CtxTokens, max(1, plan.Launch.Slots), plan.Launch.CacheTypeK, plan.Launch.CacheTypeV)
	p, err := placement.Resolve(model, kv, dev, req)
	if err != nil {
		code := "placement_unsupported"
		var pe *placement.Error
		if errors.As(err, &pe) {
			code = pe.Code
		}
		m.d.Log.Warn("The placement for "+modelID+" cannot be used: "+err.Error(), "event", "runtime.placement-refused",
			"runtime", "llm", "model_id", modelID, "mode", req.Mode, "code", code)
		return err
	}
	// One figure: a CPU load reports what admission will ask for, the same evidence and all.
	if p.GPULayers == 0 && m.d.Memory != nil {
		p.HostBytes = m.d.Memory.Projected(m.demandFor(modelID, plan, t))
		p.Note = placement.Describe(p, req.Mode)
	}
	plan.Placement, plan.PlacedAt, plan.PlacementRequest = &p, time.Now().UTC(), req
	plan.LogPlacement = !dev.NoLogVerbosity && !(p.Platform == placement.Unified && len(p.Args) == 0)
	m.d.Log.Info("Placement for "+modelID+": "+p.Note, "event", "runtime.placement", "runtime", "llm", "model_id", modelID,
		"mode", p.Mode, "platform", p.Platform, "gpu_layers", p.GPULayers, "total_layers", p.TotalLayers,
		"cpu_moe_layers", p.CPUMoELayers, "host_bytes", p.HostBytes, "gpu_bytes", p.GPUBytes, "free_vram_bytes", p.FreeVRAMBytes)
	return nil
}

// planSize and planQuant name the launch for its shape; an imported model has neither.
func planSize(m *LLM, modelID string, t *Target) string {
	if s := m.specFor(modelID, t); s != nil {
		return s.Size
	}
	return ""
}

func planQuant(m *LLM, modelID string, t *Target) string {
	if s := m.specFor(modelID, t); s != nil {
		return s.Quant
	}
	return ""
}

// placementSettle bounds the wait for llama-server's startup lines after
// /health answers (they are read from its pipes asynchronously).
var placementSettle = 2 * time.Second

// cpuPlacementSettle is the same for a plan with no GPU layers: long enough to catch
// a report that contradicts the plan, without slowing every CPU-only load.
const cpuPlacementSettle = 250 * time.Millisecond

// checkPlacement validates the backend's startup output against the plan
// and fails closed on a difference (DESIGN §20.5).
func (m *LLM) checkPlacement(proc *Process, plan *LLMPlan) error {
	p := plan.Placement
	if p == nil || (p.Platform == placement.Unified && len(p.Args) == 0) {
		return nil
	}
	settle := placementSettle
	if p.GPULayers == 0 {
		settle = cpuPlacementSettle // the line may never come, so do not wait it out
	}
	deadline := time.Now().Add(settle)
	log := proc.StartupLog()
	for !bytes.Contains(log, []byte("layers to GPU")) && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
		log = proc.StartupLog()
	}
	return placement.Check(log, *p)
}

// HostPlacement is the serve-time PlacementSource: the stored choice for
// this device, and this machine's platform, free VRAM and system headroom.
type HostPlacement struct {
	Store    *store.Store
	DeviceID string
	Memory   *memory.Controller
	Log      *slog.Logger
	// Platform and GPUs replace detection (tests).
	Platform string
	GPUs     func() []memory.GPUMemory

	helpMu sync.Mutex
	help   map[string]bool // custom llama-server build and flag → it has that flag
}

// Request reads the stored choice (auto without one).
func (h *HostPlacement) Request(modelID string) placement.Request {
	p, err := h.Store.Placement(context.Background(), modelID, h.DeviceID)
	if err != nil || p == nil {
		return placement.Request{Mode: placement.Auto}
	}
	return placement.Request{Mode: p.Mode, GPULayers: p.GPULayers, CPUMoELayers: p.CPUMoELayers}
}

// Device describes this machine for the target backend.
func (h *HostPlacement) Device(t *Target) placement.Device {
	d := placement.Device{GPULayers: true, CPUMoE: true, HostAvailable: 1 << 62}
	if h.Memory != nil {
		if room := h.Memory.Headroom(context.Background(), []string{"llm"}); room >= 0 {
			d.HostAvailable, d.KeptBytes = room, h.Memory.ReserveBytes()
			d.CapacityBytes = h.Memory.Topology().SystemCapacity
		}
	}
	cpuOnly := false
	if t != nil && t.Backend != nil {
		accelerators := map[string]bool{}
		for _, p := range t.Backend.Platforms {
			accelerators[p.Accelerator] = true
		}
		cpuOnly = len(accelerators) == 1 && accelerators["cpu"]
		if t.Backend.Launch != nil && t.Backend.Launch.Command != "" {
			d.CPUMoE = h.hasFlag(t.Backend.Launch.Command, "--n-cpu-moe")
			d.NoLogVerbosity = !h.hasFlag(t.Backend.Launch.Command, "--verbosity")
		}
	}
	platform := h.Platform
	if platform == "" && goruntime.GOOS == "darwin" && goruntime.GOARCH == "arm64" {
		platform = placement.Unified
	}
	gpuNow := memory.GPUMemoryNow
	if h.GPUs != nil {
		gpuNow = h.GPUs
	}
	switch {
	case cpuOnly:
		d.Platform, d.GPULayers = placement.CPUOnly, false
	case platform == placement.Unified:
		d.Platform = placement.Unified
	default:
		gpus := gpuNow()
		if len(gpus) == 0 {
			d.Platform, d.GPULayers = placement.CPUOnly, false
			break
		}
		d.Platform, d.GPUCount, d.FreeVRAM = placement.Discrete, len(gpus), gpus[0].FreeBytes
	}
	return d
}

// hasFlag probes a custom llama-server build's --help once.
func (h *HostPlacement) hasFlag(bin, flag string) bool {
	h.helpMu.Lock()
	defer h.helpMu.Unlock()
	if h.help == nil {
		h.help = map[string]bool{}
	}
	key := bin + "\x00" + flag
	if v, ok := h.help[key]; ok {
		return v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, bin, "--help").CombinedOutput()
	h.help[key] = bytes.Contains(out, []byte(flag))
	return h.help[key]
}

// Loading reports whether modelID is being started now (a placement change
// is refused then, DESIGN §20.8 item 6).
func (m *LLM) Loading(modelID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.starting && len(m.queue) > 0 && m.queue[0].model == modelID
}

// LoadedPlacement is the placement the running model was loaded with.
func (m *LLM) LoadedPlacement(modelID string) (*LLMPlan, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil || m.cur.modelID != modelID {
		return nil, false
	}
	return m.cur.plan, true
}

// PreviewPlacement resolves req for modelID now, without loading anything:
// what the next load would do.
func (m *LLM) PreviewPlacement(modelID string, req placement.Request) (*LLMPlan, error) {
	if m.d.Placement == nil {
		return nil, errors.New("placement planning is not available")
	}
	t, _ := m.target(modelID)
	plan, err := m.planFor(modelID, t)
	if err != nil {
		return nil, err
	}
	if err := m.placeWith(modelID, plan, t, req); err != nil {
		return nil, err
	}
	return plan, nil
}

// cpuOnly reports a machine and backend that cannot take any layers on a GPU.
func (m *LLM) cpuOnly(t *Target) bool {
	return m.d.Placement != nil && m.d.Placement.Device(t).Platform == placement.CPUOnly
}

// NextLoadBytes is what the model's next load would count for against this computer's
// memory: the figure admission uses. ok is false when that cannot be told.
func (m *LLM) NextLoadBytes(modelID string) (need int64, ok bool) {
	if m.d.Memory == nil {
		return 0, false
	}
	t, _ := m.target(modelID)
	plan, err := m.planFor(modelID, t)
	if err != nil {
		return 0, false
	}
	if m.d.Placement != nil {
		_ = m.place(modelID, plan, t) // a refusal still leaves a figure
	}
	return m.d.Memory.Projected(m.demandFor(modelID, plan, t)), true
}

// SetPlacementSource sets the placement source (before any load).
func (m *LLM) SetPlacementSource(p PlacementSource) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.d.Placement = p
}
