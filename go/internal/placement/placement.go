// Package placement plans where an LLM's tensors live, GPU or host memory,
// so that it fits (DESIGN §20). It is capacity placement, not a speed
// choice. The planner is pure: it reads the GGUF tensor table and the launch
// geometry, never the weights.
package placement

import (
	"fmt"
	"os"
	"regexp"
	"strconv"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

// Modes (DESIGN §20.1).
const (
	Auto   = "auto"
	GPU    = "gpu"
	CPU    = "cpu"
	Custom = "custom"
)

// Platforms.
const (
	Unified  = "unified"         // Apple Silicon: GPU and CPU share memory
	Discrete = "discrete-nvidia" // a discrete NVIDIA GPU (Linux)
	CPUOnly  = "cpu"             // no GPU, or a CPU-only backend
)

// Margins, as admission uses them (DESIGN §20.3).
const (
	WorkspaceBytes = 256 << 20 // compute buffers and scratch on the GPU
	ReserveBytes   = 512 << 20 // free VRAM left untouched
)

// Tensor is one GGUF tensor-table entry.
type Tensor struct {
	Name string
	Dims []uint64
	Type uint32
}

// Model is what the planner needs from a GGUF header.
type Model struct {
	Tensors       []Tensor
	ArtifactBytes int64 // the file, a conservative bound on resident weights
	Layers        int   // repeating blocks (block_count)
}

// Device is the machine and backend the model would run on.
type Device struct {
	Platform      string
	FreeVRAM      int64 // free VRAM on GPU 0 (discrete)
	HostAvailable int64 // usable system memory for the host side
	GPUCount      int   // NVIDIA GPUs seen; more than one is refused
	CPUMoE        bool  // the backend has --n-cpu-moe
	GPULayers     bool  // the backend can offload layers (-ngl)
}

// Request is the stored choice.
type Request struct {
	Mode         string `json:"mode"`
	CPUMoELayers *int   `json:"cpuMoeLayers,omitempty"`
	GPULayers    *int   `json:"gpuLayers,omitempty"`
}

// Plan is the resolved split.
type Plan struct {
	Mode           string   `json:"mode"`
	Platform       string   `json:"platform"`
	Pool           string   `json:"pool"`
	GPULayers      int      `json:"gpuLayers"`
	TotalLayers    int      `json:"totalLayers"`
	CPUMoELayers   int      `json:"cpuMoeLayers"`
	HostTensors    int      `json:"hostTensors"`
	HostLayers     []int    `json:"hostLayers"`
	GPUBytes       int64    `json:"gpuBytes"`
	HostBytes      int64    `json:"hostBytes"`
	KVBytes        int64    `json:"kvBytes"`
	WorkspaceBytes int64    `json:"workspaceBytes"`
	ReserveBytes   int64    `json:"reserveBytes"`
	FreeVRAMBytes  int64    `json:"freeVramBytes"`
	Args           []string `json:"-"`
	// Note says, in product words, what was decided and why.
	Note string `json:"note"`
	// HostExperts are the expert tensors kept in host memory, for the
	// startup placement check.
	HostExperts []string `json:"-"`
}

// Error is an unsupported or unfittable placement.
type Error struct {
	Code    string // placement_unsupported | placement_does_not_fit
	Message string
}

func (e *Error) Error() string { return e.Message }

func unsupported(format string, a ...any) error {
	return &Error{"placement_unsupported", fmt.Sprintf(format, a...)}
}

// --- tensor sizes -----------------------------------------------------------------

// ggml block sizes: bytes per block and values per block, by type id.
var typeSizes = map[uint32][2]int64{
	0: {4, 1}, 1: {2, 1}, 2: {18, 32}, 3: {20, 32}, 6: {22, 32}, 7: {24, 32}, 8: {34, 32}, 9: {36, 32},
	10: {84, 256}, 11: {110, 256}, 12: {144, 256}, 13: {176, 256}, 14: {210, 256}, 15: {292, 256},
	16: {66, 256}, 17: {74, 256}, 18: {98, 256}, 19: {50, 256}, 20: {18, 32}, 21: {110, 256},
	22: {82, 256}, 23: {136, 256}, 24: {1, 1}, 25: {2, 1}, 26: {4, 1}, 27: {8, 1}, 28: {8, 1},
	29: {56, 256}, 30: {2, 1}, 34: {54, 256}, 35: {66, 256}, 39: {17, 32},
}

// TensorBytes is a tensor's size in the file.
func TensorBytes(t Tensor) (int64, bool) {
	s, ok := typeSizes[t.Type]
	if !ok {
		return 0, false
	}
	n := int64(1)
	for _, d := range t.Dims {
		n *= int64(d)
	}
	return n / s[1] * s[0], n%s[1] == 0
}

// --- experts and layers -----------------------------------------------------------

// llama.cpp's --n-cpu-moe pattern (common.h LLM_FFN_EXPS_REGEX), per layer.
var expertRe = regexp.MustCompile(`^blk\.(\d+)\.ffn_(up|down|gate|gate_up)_(ch|)exps`)
var layerRe = regexp.MustCompile(`^blk\.(\d+)\.`)

// ExpertSet is a model's expert tensors by layer.
type ExpertSet struct {
	Tensors int
	ByLayer map[int]int64    // layer → expert bytes
	Names   map[int][]string // layer → expert tensor names
}

// Experts finds the tensors --n-cpu-moe keeps on the host.
func Experts(ts []Tensor) ExpertSet {
	e := ExpertSet{ByLayer: map[int]int64{}, Names: map[int][]string{}}
	for _, t := range ts {
		m := expertRe.FindStringSubmatch(t.Name)
		if m == nil {
			continue
		}
		l, _ := strconv.Atoi(m[1])
		b, _ := TensorBytes(t)
		e.Tensors++
		e.ByLayer[l] += b
		e.Names[l] = append(e.Names[l], t.Name)
	}
	return e
}

type sizes struct {
	layer     []int64 // per repeating layer
	output    int64   // output.* (offloaded with the last -ngl step)
	experts   ExpertSet
	unknownTy bool
}

func measure(m Model) sizes {
	s := sizes{layer: make([]int64, m.Layers), experts: Experts(m.Tensors)}
	for _, t := range m.Tensors {
		b, ok := TensorBytes(t)
		if !ok {
			s.unknownTy = true
		}
		if mm := layerRe.FindStringSubmatch(t.Name); mm != nil {
			if l, _ := strconv.Atoi(mm[1]); l < m.Layers {
				s.layer[l] += b
			}
		} else if len(t.Name) >= 7 && t.Name[:7] == "output." {
			s.output += b
		}
	}
	return s
}

// split is one candidate: nMoE layers' experts on the host, gpuLayers of
// the layer count (repeating layers from the end, then the output) on the
// GPU.
type split struct {
	nMoE, gpuLayers int
	gpu, host, kv   int64
}

func (s sizes) evaluate(m Model, kv int64, nMoE, gpuLayers int) split {
	total := m.Layers + 1
	sp := split{nMoE: nMoE, gpuLayers: gpuLayers}
	if gpuLayers == 0 {
		sp.host = m.ArtifactBytes + kv
		return sp
	}
	hostExperts := int64(0)
	for l := 0; l < nMoE; l++ {
		hostExperts += s.experts.ByLayer[l]
	}
	var weights, kvGPU int64
	if gpuLayers >= total {
		// The whole file is the conservative bound on the GPU side.
		weights, kvGPU = m.ArtifactBytes-hostExperts, kv
	} else {
		rep := min(gpuLayers, m.Layers)
		for l := m.Layers - rep; l < m.Layers; l++ {
			weights += s.layer[l]
			if l < nMoE {
				weights -= s.experts.ByLayer[l]
			}
		}
		if m.Layers > 0 {
			kvGPU = kv * int64(rep) / int64(m.Layers)
		}
	}
	sp.gpu = weights + kvGPU + WorkspaceBytes
	sp.host = m.ArtifactBytes - weights + kv - kvGPU
	sp.kv = kv
	return sp
}

func (sp split) fits(d Device) bool {
	gpuOK := sp.gpuLayers == 0 || sp.gpu+ReserveBytes <= d.FreeVRAM
	return gpuOK && sp.host <= d.HostAvailable
}

// --- resolve ----------------------------------------------------------------------

// Resolve turns a request into a plan for this device, or an *Error.
func Resolve(m Model, kv int64, d Device, r Request) (Plan, error) {
	if r.Mode == "" {
		r.Mode = Auto
	}
	switch r.Mode {
	case Auto, GPU, CPU, Custom:
	default:
		return Plan{}, unsupported(`Unknown placement mode %q. Use auto, gpu, cpu or custom.`, r.Mode)
	}
	if d.GPUCount > 1 {
		return Plan{}, unsupported("This machine has %d NVIDIA GPUs. LocalBase places a model on one GPU only: start the gateway with CUDA_VISIBLE_DEVICES set to a single GPU (for example CUDA_VISIBLE_DEVICES=0), or choose CPU-only placement.", d.GPUCount)
	}
	total := m.Layers + 1
	s := measure(m)
	p := Plan{Mode: r.Mode, Platform: d.Platform, Pool: "system", TotalLayers: total, KVBytes: kv, HostLayers: []int{}}
	if d.Platform == Discrete {
		p.Pool, p.FreeVRAMBytes = "gpu:0", d.FreeVRAM
	}
	nMoE, layers := 0, total
	if r.Mode == Custom {
		if r.CPUMoELayers != nil {
			nMoE = *r.CPUMoELayers
			switch {
			case nMoE < 0:
				return Plan{}, unsupported("CPU expert layers must be 0 or more.")
			case nMoE > 0 && d.Platform == Unified:
				return Plan{}, unsupported("On unified memory, keeping experts in CPU memory frees no memory; this setting applies to discrete GPUs.")
			case nMoE > 0 && d.Platform == CPUOnly:
				return Plan{}, unsupported("This model runs on the CPU here, so all of it is already in system memory; CPU expert layers apply to discrete GPUs.")
			case nMoE > 0 && len(s.experts.ByLayer) == 0:
				return Plan{}, unsupported("This model has no expert layers, so CPU expert layers do not apply. Use GPU layers for a partial offload instead.")
			case nMoE > 0 && !d.CPUMoE:
				return Plan{}, unsupported("This backend cannot keep expert layers in CPU memory (no --n-cpu-moe). Use GPU layers, or choose a backend that supports it.")
			case nMoE > len(s.experts.ByLayer):
				return Plan{}, unsupported("This model has %d expert layers; CPU expert layers must be from 0 to %d.", len(s.experts.ByLayer), len(s.experts.ByLayer))
			}
		}
		if r.GPULayers != nil {
			layers = *r.GPULayers
			switch {
			case layers < 0 || layers > total:
				return Plan{}, unsupported("GPU layers must be from 0 to %d for this model.", total)
			case layers > 0 && d.Platform == CPUOnly:
				return Plan{}, unsupported("This backend runs on the CPU only, so GPU layers must be 0. Choose a GPU backend to offload layers.")
			case layers < total && !d.GPULayers && d.Platform != CPUOnly:
				return Plan{}, unsupported("This backend cannot offload part of the model (-ngl).")
			}
		}
	}
	if r.Mode == GPU && d.Platform == CPUOnly {
		return Plan{}, unsupported("This backend runs on the CPU only. Choose a GPU backend for GPU-only placement, or use auto or CPU-only.")
	}

	if d.Platform == Unified {
		switch r.Mode {
		case CPU:
			layers = 0
		case Auto, GPU:
			layers = total
		}
		p.GPULayers = layers
		if layers < total {
			p.Args = []string{"-ngl", strconv.Itoa(layers)}
		}
		p.Note = "Unified memory: the GPU and CPU share the same memory, so offloading frees nothing; the model is admitted against system memory."
		sp := s.evaluate(m, kv, 0, layers)
		p.GPUBytes, p.HostBytes = sp.gpu, sp.host
		if layers == total {
			p.HostBytes = 0
		}
		return p, nil
	}
	if d.Platform == CPUOnly {
		layers = 0
	}

	var chosen *split
	switch {
	case r.Mode == CPU || d.Platform == CPUOnly:
		sp := s.evaluate(m, kv, 0, 0)
		chosen = &sp
		if !sp.fits(d) {
			return Plan{}, doesNotFit(sp, d)
		}
	case r.Mode == GPU:
		sp := s.evaluate(m, kv, 0, total)
		if !sp.fits(d) {
			return Plan{}, doesNotFit(sp, d)
		}
		chosen = &sp
	case r.Mode == Custom:
		sp := s.evaluate(m, kv, nMoE, layers)
		if !sp.fits(d) {
			return Plan{}, doesNotFit(sp, d)
		}
		chosen = &sp
	default: // auto
		if s.unknownTy {
			// Sizes unknown: keep today's full-GPU launch and let admission decide.
			sp := s.evaluate(m, kv, 0, total)
			chosen = &sp
			p.Note = "Some tensor types in this file are not sized by the planner, so it stays fully on the GPU."
			break
		}
		try := func(n, l int) bool {
			sp := s.evaluate(m, kv, n, l)
			if sp.fits(d) {
				chosen = &sp
				return true
			}
			return false
		}
		found := try(0, total)
		for n := 1; !found && n <= len(s.experts.ByLayer) && d.CPUMoE; n++ {
			found = try(n, total)
		}
		nMax := 0
		if d.CPUMoE {
			nMax = len(s.experts.ByLayer)
		}
		for l := total - 1; !found && l >= 0 && d.GPULayers; l-- {
			found = try(nMax, l)
		}
		if !found {
			return Plan{}, doesNotFit(s.evaluate(m, kv, 0, total), d)
		}
	}
	sp := *chosen
	p.GPULayers, p.CPUMoELayers = sp.gpuLayers, sp.nMoE
	p.GPUBytes, p.HostBytes = sp.gpu, sp.host
	if sp.gpuLayers > 0 {
		p.WorkspaceBytes, p.ReserveBytes = WorkspaceBytes, ReserveBytes
	}
	for l := 0; l < sp.nMoE; l++ {
		p.HostLayers = append(p.HostLayers, l)
		p.HostTensors += len(s.experts.Names[l])
		p.HostExperts = append(p.HostExperts, s.experts.Names[l]...)
	}
	if sp.nMoE > 0 {
		p.Args = append(p.Args, "--n-cpu-moe", strconv.Itoa(sp.nMoE))
	}
	if sp.gpuLayers < total {
		p.Args = append(p.Args, "-ngl", strconv.Itoa(sp.gpuLayers))
	}
	if p.Note == "" {
		p.Note = describe(p, r.Mode)
	}
	return p, nil
}

func doesNotFit(sp split, d Device) error {
	gib := func(b int64) string { return strconv.FormatFloat(float64(b)/(1<<30), 'f', 1, 64) + " GB" }
	if sp.gpuLayers > 0 && sp.gpu+ReserveBytes > d.FreeVRAM {
		return &Error{"placement_does_not_fit", fmt.Sprintf("This placement needs %s of GPU memory (including the %s reserve), but %s is free. Choose auto, keep more layers or experts in CPU memory, or lower the context size.",
			gib(sp.gpu+ReserveBytes), gib(ReserveBytes), gib(d.FreeVRAM))}
	}
	return &Error{"placement_does_not_fit", fmt.Sprintf("This placement needs %s of system memory, but %s is available. Keep more of the model on the GPU, close other applications, or lower the context size.",
		gib(sp.host), gib(d.HostAvailable))}
}

// describe is the factual line for a discrete placement: capacity and CPU
// use only.
func describe(p Plan, mode string) string {
	gb := strconv.FormatFloat(float64(p.HostBytes)/(1<<30), 'f', 1, 64) + " GB"
	switch {
	case p.GPULayers == 0:
		return "Runs on the CPU; uses " + gb + " of system memory."
	case p.CPUMoELayers == 0 && p.GPULayers == p.TotalLayers:
		if mode == Auto {
			return "Fits on the GPU; nothing is kept in system memory."
		}
		return "Runs fully on the GPU."
	case p.CPUMoELayers > 0 && p.GPULayers == p.TotalLayers:
		why := ""
		if mode == Auto {
			why = " The full model does not fit in free GPU memory, so auto kept the smallest number of expert layers in CPU memory."
		}
		return fmt.Sprintf("Experts of layers 0–%d stay in system memory (%s); everything else is on the GPU.%s", p.CPUMoELayers-1, gb, why)
	default:
		why := ""
		if mode == Auto {
			why = " The full model does not fit in free GPU memory, so auto offloaded as many layers as fit."
		}
		return fmt.Sprintf("%d of %d layers on the GPU; the rest stays in system memory (%s).%s", p.GPULayers, p.TotalLayers, gb, why)
	}
}

// LoadModel reads a GGUF file's tensor table (no weights) for the planner.
func LoadModel(path string, layers int) (Model, error) {
	ts, err := memory.ReadTensorTable(path)
	if err != nil {
		return Model{}, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return Model{}, err
	}
	m := Model{ArtifactBytes: st.Size(), Layers: layers}
	for _, t := range ts {
		m.Tensors = append(m.Tensors, Tensor{Name: t.Name, Dims: t.Dims, Type: t.Type})
	}
	return m, nil
}
