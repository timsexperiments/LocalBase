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
	plan.Placement, plan.PlacedAt, plan.PlacementRequest = &p, time.Now().UTC(), req
	m.d.Log.Info("Placement for "+modelID+": "+p.Note, "event", "runtime.placement", "runtime", "llm", "model_id", modelID,
		"mode", p.Mode, "platform", p.Platform, "gpu_layers", p.GPULayers, "total_layers", p.TotalLayers,
		"cpu_moe_layers", p.CPUMoELayers, "host_bytes", p.HostBytes, "gpu_bytes", p.GPUBytes, "free_vram_bytes", p.FreeVRAMBytes)
	return nil
}

// placementSettle bounds the wait for llama-server's startup lines after
// /health answers (they are read from its pipes asynchronously).
var placementSettle = 2 * time.Second

// checkPlacement validates the backend's startup output against the plan
// and fails closed on a difference (DESIGN §20.5).
func (m *LLM) checkPlacement(proc *Process, plan *LLMPlan) error {
	p := plan.Placement
	if p == nil || (p.Platform == placement.Unified && len(p.Args) == 0) {
		return nil
	}
	deadline := time.Now().Add(placementSettle)
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
	help   map[string]bool // custom llama-server build → has --n-cpu-moe
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
			d.HostAvailable = room
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
	if v, ok := h.help[bin]; ok {
		return v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, bin, "--help").CombinedOutput()
	h.help[bin] = bytes.Contains(out, []byte(flag))
	return h.help[bin]
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

// SetPlacementSource sets the placement source (before any load).
func (m *LLM) SetPlacementSource(p PlacementSource) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.d.Placement = p
}
