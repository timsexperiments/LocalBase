package runtime

import (
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

const (
	MaxParallelSlots       = 4
	MinContextPerSlot      = 2048
	reservedRuntimeGB      = 2
	contextGBPer8K         = 0.5
	parallelSlotOverheadGB = 0.5
	// DecisionContextTokens caps a decision model's per-slot context: the
	// whole prompt is one micro-batch, so compute grows with it (#353).
	DecisionContextTokens = 8192
	PromptCacheRAMMiB     = 2048
	DefaultKVCacheType    = "q8_0"
)

// Parallel is the resolved slot allocation.
type Parallel struct {
	Slots          int
	IsAuto         bool
	ContextPerSlot int
}

// AllocateParallel ports allocateParallelSlots.
func AllocateParallel(setting string, memoryGB, modelGB float64, ctx int, kvGB func(int) float64) (Parallel, error) {
	if ctx < MinContextPerSlot {
		return Parallel{}, fmt.Errorf("context size must be at least %d tokens", MinContextPerSlot)
	}
	if setting != "auto" {
		n, err := strconv.Atoi(setting)
		if err != nil || n < 1 || n > MaxParallelSlots {
			return Parallel{}, fmt.Errorf("invalid parallel slots %q", setting)
		}
		if ctx < n*MinContextPerSlot {
			return Parallel{}, fmt.Errorf("parallel slots %d require at least %d total context tokens", n, n*MinContextPerSlot)
		}
		return Parallel{Slots: n, ContextPerSlot: ctx / n}, nil
	}
	ctxSlots := max(1, min(MaxParallelSlots, ctx/MinContextPerSlot))
	if memoryGB <= 0 || modelGB < 0 {
		return Parallel{Slots: 1, IsAuto: true, ContextPerSlot: ctx}, nil
	}
	avail := memoryGB - modelGB - reservedRuntimeGB
	memSlots := 1
	for s := MaxParallelSlots; s > 1; s-- {
		ctxGB := float64(ctx) / 8192 * contextGBPer8K
		if kvGB != nil {
			ctxGB = kvGB(s)
		}
		if avail-ctxGB >= float64(s)*parallelSlotOverheadGB {
			memSlots = s
			break
		}
	}
	slots := min(ctxSlots, memSlots)
	return Parallel{Slots: slots, IsAuto: true, ContextPerSlot: ctx / slots}, nil
}

// RecommendedContext ports calculateMaxSafeContextSize.
func RecommendedContext(spec *catalog.Spec, memoryGB float64) int {
	if spec == nil {
		if memoryGB >= 32 {
			return 32768
		}
		return 8192
	}
	p, err := strconv.ParseFloat(leadingNumber(spec.Size), 64)
	if err != nil {
		return 8192
	}
	avail := (memoryGB-spec.StorageGB)*0.8 - 4.0
	if avail <= 0 {
		return 4096
	}
	c := avail / (p * 8e-6)
	rec := 4096
	for _, b := range []int{4096, 8192, 16384, 24576, 32768, 49152, 65536, 98304, 131072} {
		if float64(b) <= c {
			rec = b
		}
	}
	older := strings.Contains(strings.ToLower(spec.Family), "phi-3") ||
		strings.Contains(spec.ModelID, "llama-3.2-") || strings.Contains(spec.ModelID, "llama-3-8b")
	maxCtx := 131072
	if older {
		maxCtx = 8192
	}
	out := min(rec, maxCtx)
	if spec.ContextWindowTokens != nil {
		out = min(out, *spec.ContextWindowTokens)
	}
	return out
}

// leadingNumber mimics JS parseFloat: the numeric prefix of s.
func leadingNumber(s string) string {
	end := 0
	dot := false
	for end < len(s) && (s[end] >= '0' && s[end] <= '9' || s[end] == '.' && !dot) {
		if s[end] == '.' {
			dot = true
		}
		end++
	}
	return s[:end]
}

// LLMPlan is everything needed to launch and admit llama-server.
type LLMPlan struct {
	ModelID        string
	ModelPath      string
	ProjectorPath  string
	Host           string
	Port           int
	CtxSize        int
	Parallel       Parallel
	Embedding      bool
	Pooling        string
	Decision       bool
	KVType         string
	PromptCacheMiB int
	Geometry       *memory.Geometry
	ArtifactBytes  int64
	Demand         memory.Demand
	Budget         ContextBudget
	Launch         memory.LLMLaunch
}

// ContextBudget is the `context_budget` object in /v1/models.
type ContextBudget struct {
	Mode               string   `json:"mode"`
	ConfiguredCeiling  int      `json:"configured_ceiling"`
	RecommendedMax     int      `json:"recommended_max"`
	ProcessOverride    *int     `json:"process_override"`
	ModelContextWindow *int     `json:"model_context_window"`
	TrainingContext    *int     `json:"training_context"`
	LaunchContext      int      `json:"launch_context"`
	ParallelSlots      int      `json:"parallel_slots"`
	EffectiveContext   int      `json:"effective_context"`
	Reasons            []string `json:"reasons"`
}

// PlanInput carries config and host facts into PlanLLM.
type PlanInput struct {
	Config      *store.Config
	Spec        *catalog.Spec // nil for an uncatalogued file
	ModelID     string
	ModelsDir   string
	MemoryGB    float64
	CtxOverride int // serve --ctx-size; 0 = none
	Port        int
	// ReadHeader reads the GGUF geometry; false skips file reads (listing).
	ReadHeader bool
	// Geometry, when set, is used instead of reading the header.
	Geometry *memory.Geometry
	// ModelFile overrides the catalog's primary file (serve --llm-model-file).
	ModelFile string
	// For a model without a catalog entry (imports): its task, projector
	// path and weight bytes.
	Task          string
	Projector     string
	ArtifactBytes int64
}

func fmtInt(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// PlanLLM ports resolveConfiguredLlmLaunchPlan + resolveLlmLaunchPlan.
func PlanLLM(in PlanInput) (*LLMPlan, error) {
	spec, cfg := in.Spec, in.Config
	p := &LLMPlan{ModelID: in.ModelID, Host: BackendHost(cfg.Host), Port: in.Port}
	modelFile := in.ModelID + ".gguf"
	if spec == nil && fileExists(filepath.Join(in.ModelsDir, in.ModelID+".bin")) {
		modelFile = in.ModelID + ".bin"
	}
	if spec != nil {
		modelFile = spec.Primary().Filename
		p.ArtifactBytes = spec.TotalArtifactBytes()
		p.Embedding = spec.IsEmbedding()
		p.Decision = spec.IsDecision()
		if p.Embedding {
			p.Pooling = spec.LLMRuntime.Pooling
		}
		if a := spec.Projector(); a != nil {
			if path := filepath.Join(in.ModelsDir, a.Filename); fileExists(path) {
				p.ProjectorPath = path
			}
		}
	}
	if in.ModelFile != "" {
		modelFile = in.ModelFile
	}
	if spec == nil {
		p.Embedding, p.Decision = in.Task == "embeddings", in.Task == "decision"
		p.ArtifactBytes = in.ArtifactBytes
		if in.Projector != "" && fileExists(in.Projector) {
			p.ProjectorPath = in.Projector
		}
	}
	p.ModelPath = filepath.Join(in.ModelsDir, modelFile)
	if p.ArtifactBytes == 0 {
		p.ArtifactBytes = fileSize(p.ModelPath)
	}
	if in.Geometry != nil {
		p.Geometry = in.Geometry
	} else if in.ReadHeader {
		p.Geometry = memory.ReadGeometry(p.ModelPath)
	}

	// Context: override > explicit ceiling > min(recommended, ceiling).
	recommended := RecommendedContext(spec, in.MemoryGB)
	ctx := min(recommended, cfg.CtxSize)
	if cfg.CtxMode == "explicit" {
		ctx = cfg.CtxSize
	}
	if in.CtxOverride > 0 {
		ctx = in.CtxOverride
	}
	preWindow := ctx
	window := ctx
	if spec != nil && spec.ContextWindowTokens != nil {
		window = min(ctx, *spec.ContextWindowTokens)
	}

	p.KVType = DefaultKVCacheType
	if p.Embedding || p.Geometry == nil || !p.Geometry.Q8Compatible {
		p.KVType = "f16"
	}
	if !p.Embedding && !p.Decision {
		p.PromptCacheMiB = PromptCacheRAMMiB
	}
	var kvGB func(int) float64
	switch {
	case p.Decision:
		kvGB = func(int) float64 { return 0 }
	case p.Geometry != nil:
		g := p.Geometry
		kvGB = func(s int) float64 {
			return float64(memory.KVCacheBytes(g, window, s, p.KVType, p.KVType)) / float64(memory.GiB)
		}
	}
	par, err := AllocateParallel(cfg.Parallel, in.MemoryGB, float64(p.ArtifactBytes)/float64(memory.GiB), window, kvGB)
	if err != nil {
		return nil, err
	}
	p.CtxSize = window
	if p.Decision {
		p.CtxSize = min(window, DecisionContextTokens*par.Slots)
		par.ContextPerSlot = min(par.ContextPerSlot, DecisionContextTokens)
	}
	p.Parallel = par
	p.Launch = memory.LLMLaunch{CtxTokens: p.CtxSize, Slots: par.Slots, CacheTypeK: p.KVType, CacheTypeV: p.KVType,
		PromptCacheBytes: int64(p.PromptCacheMiB) * memory.MiB, Decision: p.Decision}

	// Estimate, replaced by a measurement at the exact shape for qualified entries.
	var size, quant string
	if spec != nil {
		size, quant = spec.Size, spec.Quant
	}
	p.Demand = memory.EstimateLLM(p.ArtifactBytes, memory.ResolveShape(size, quant, p.ArtifactBytes, p.Geometry), p.Launch)
	if spec != nil && spec.Kind == "llm" && spec.Qualified() {
		if d, ok := memory.MeasuredLLMDemand(spec.ModelID, p.Launch, p.ArtifactBytes); ok {
			p.Demand = d
		}
	}

	// Budget explanation.
	b := ContextBudget{Mode: cfg.CtxMode, ConfiguredCeiling: cfg.CtxSize, RecommendedMax: recommended,
		LaunchContext: p.CtxSize, ParallelSlots: par.Slots, Reasons: []string{}}
	if in.CtxOverride > 0 {
		v := in.CtxOverride
		b.ProcessOverride = &v
		b.Reasons = append(b.Reasons, "Process override (--ctx-size/LOCALBASE_CTX_SIZE) set the launch context to "+fmtInt(v)+"; it does not persist across managed service restarts.")
	} else if cfg.CtxMode != "explicit" && recommended < cfg.CtxSize {
		b.Reasons = append(b.Reasons, `Context mode "auto" reduced the `+fmtInt(cfg.CtxSize)+" ceiling to the memory-safe recommendation of "+fmtInt(recommended)+`; run "local-base configure --ctx-mode explicit" to launch at the ceiling.`)
	} else if cfg.CtxMode == "explicit" && cfg.CtxSize > recommended {
		b.Reasons = append(b.Reasons, `Context mode "explicit" launches at `+fmtInt(cfg.CtxSize)+", above the memory-safe recommendation of "+fmtInt(recommended)+".")
	}
	if spec != nil && spec.ContextWindowTokens != nil {
		w := *spec.ContextWindowTokens
		b.ModelContextWindow = &w
		if w < preWindow {
			b.Reasons = append(b.Reasons, "The model's advertised context window caps the launch context at "+fmtInt(w)+".")
		}
	}
	if p.Decision && DecisionContextTokens*par.Slots < window {
		b.Reasons = append(b.Reasons, "Decision models evaluate a prompt as one micro-batch, so each slot is capped at "+fmtInt(DecisionContextTokens)+" tokens to bound the compute buffer.")
	}
	perSlot := memory.ContextPerSequence(p.CtxSize, par.Slots)
	if par.Slots > 1 {
		auto := ""
		if par.IsAuto {
			auto = " (parallel = auto)"
		}
		b.Reasons = append(b.Reasons, fmt.Sprintf("%d parallel slots%s divide the %s launch context into %s tokens per request; set parallel = 1 to give a single request the full context.", par.Slots, auto, fmtInt(p.CtxSize), fmtInt(perSlot)))
	}
	b.EffectiveContext = perSlot
	if p.Geometry != nil && p.Geometry.ContextLength > 0 {
		t := p.Geometry.ContextLength
		b.TrainingContext = &t
		if t < perSlot {
			b.Reasons = append(b.Reasons, "The model file's training context limits each request to "+fmtInt(t)+" tokens.")
			b.EffectiveContext = t
		}
	}
	p.Budget = b
	return p, nil
}

// Args builds llama-server argv, identical to buildLlamaServerArgs.
func (p *LLMPlan) Args(darwinArm64 bool) []string {
	a := []string{"-m", p.ModelPath, "--host", p.Host, "--port", strconv.Itoa(p.Port),
		"-c", strconv.Itoa(p.CtxSize), "--parallel", strconv.Itoa(p.Parallel.Slots), "--no-kv-unified"}
	if p.ProjectorPath != "" {
		a = append(a, "--mmproj", p.ProjectorPath)
	}
	switch {
	case p.Decision:
		per := strconv.Itoa(memory.ContextPerSequence(p.CtxSize, p.Parallel.Slots))
		a = append(a, "-b", per, "-ub", per, "--flash-attn", "on")
	case p.Embedding:
		a = append(a, "--embedding")
		if p.Pooling != "" { // imported models: the GGUF header decides
			a = append(a, "--pooling", p.Pooling)
		}
		if darwinArm64 {
			a = append(a, "--flash-attn", "auto")
		}
	default:
		a = append(a, "--jinja", "--embeddings", "--flash-attn", "on")
	}
	a = append(a, "--cache-type-k", p.KVType, "--cache-type-v", p.KVType, "--cache-ram", strconv.Itoa(p.PromptCacheMiB))
	if !p.Embedding && !p.Decision {
		a = append(a, "--cache-reuse", "256")
	}
	return a
}

// BackendHost keeps unauthenticated backends on loopback (wildcards included).
func BackendHost(host string) string {
	h := strings.TrimSpace(host)
	if strings.EqualFold(strings.TrimSuffix(h, "."), "localhost") {
		return h
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	h = strings.SplitN(h, "%", 2)[0]
	if strings.EqualFold(h, "::1") {
		return "::1"
	}
	parts := strings.Split(h, ".")
	if len(parts) == 4 && parts[0] == "127" {
		ok := true
		for _, x := range parts {
			n, err := strconv.Atoi(x)
			ok = ok && err == nil && n >= 0 && n <= 255
		}
		if ok {
			return h
		}
	}
	return "127.0.0.1"
}

// ApplyObserved applies the admission order observed > measured > feed > estimate.
// An observed macOS footprint peak at the exact sha and shape wins outright.
func ApplyObserved(d memory.Demand, peaks []store.ObservedPeak, shape string) memory.Demand {
	for _, pk := range peaks {
		if pk.Method == "footprint" && pk.PeakBytes > 0 && pk.LaunchShape == shape {
			b := int64(math.Ceil(float64(pk.PeakBytes + memory.ObservedMargin(pk.PeakBytes))))
			return memory.Demand{UnifiedBytes: b, HostBytes: b, AcceleratorBytes: b, Confidence: "observed"}
		}
	}
	return d
}
