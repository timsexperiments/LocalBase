package memory

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Demand is a runtime's memory reservation per pool view.
type Demand struct {
	UnifiedBytes     int64
	HostBytes        int64
	AcceleratorBytes int64
	// Confidence: derived (GGUF geometry), estimated (size fallback),
	// measured (checked-in record), feed, or observed (this machine's peak).
	Confidence string
}

const (
	MiB = int64(1) << 20
	GiB = int64(1) << 30

	RuntimeHostOverhead = 512 * MiB
	AcceleratorOverhead = 512 * MiB
	MinimumMarginBytes  = 512 * MiB
	MinimumMarginRatio  = 0.1

	llamaUbatchTokens          = 512
	llamaLiveEmbdActivations   = 4
	decisionLiveEmbdActivation = 16
	llamaSlotBytesPerVocab     = 16
	llamaSlotFixedBytes        = 16 * MiB

	// LLMReferenceContext is the measured reference launch: 8K, one slot.
	LLMReferenceContext = 8192
)

// MinimumMemoryGB is whole GiB holding bytes plus max(512 MiB, 10%).
func MinimumMemoryGB(bytes int64) int {
	req := float64(bytes) + math.Max(float64(MinimumMarginBytes), float64(bytes)*MinimumMarginRatio)
	return max(1, int(math.Ceil(req/float64(GiB))))
}

// ObservedMargin is the margin added to an observed peak for admission.
func ObservedMargin(peak int64) int64 {
	return int64(math.Max(float64(MinimumMarginBytes), float64(peak)*MinimumMarginRatio))
}

// LLMShape: geometry plus allowances for compute and slot terms.
type LLMShape struct {
	Geometry     *Geometry
	QueryHeads   int
	EmbeddingLen int
	VocabSize    int
	FromGGUF     bool
}

var quantBits = map[string]float64{
	"Q4_0": 4.55, "Q4_K_M": 4.89, "Q5_0": 5.54, "Q5_K_M": 5.69, "Q6_K": 6.56, "Q8_0": 8.5, "F16": 16, "BF16": 16,
}

var sizeRe = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*B`)

func parameterBillions(size string, weights int64, quant string) float64 {
	if m := sizeRe.FindStringSubmatch(size); m != nil {
		v, _ := strconv.ParseFloat(m[1], 64)
		return v
	}
	bpw, ok := quantBits[strings.ToUpper(quant)]
	if !ok {
		bpw = 4.89
	}
	return float64(weights) * 8 / bpw / 1e9
}

// FallbackShape is the size-based shape when the GGUF geometry is unknown.
func FallbackShape(paramsB float64) LLMShape {
	p := math.Max(0.1, paramsB)
	// The epsilon absorbs last-bit pow differences from V8 (32^0.4 is exactly 4 there).
	layers := int(math.Ceil(16*math.Pow(p, 0.4) - 1e-9))
	embd := int(math.Round(math.Sqrt(p * 1e9 / (12 * float64(layers)))))
	return LLMShape{
		Geometry: &Geometry{Architecture: "llama", BlockCount: layers, FullKVHeads: layers * 8,
			KeyLength: 128, ValueLength: 128, Q8Compatible: true},
		QueryHeads: max(1, int(math.Round(float64(embd)/128))), EmbeddingLen: embd, VocabSize: 152064,
	}
}

// ResolveShape prefers the file's geometry; compute terms keep the fallback's allowances.
func ResolveShape(size, quant string, weights int64, g *Geometry) LLMShape {
	s := FallbackShape(parameterBillions(size, weights, quant))
	if g != nil {
		s.Geometry, s.FromGGUF = g, true
	}
	return s
}

// LLMLaunch is the shape that drives the estimate and record matching.
type LLMLaunch struct {
	CtxTokens        int
	Slots            int
	CacheTypeK       string
	CacheTypeV       string
	PromptCacheBytes int64
	Decision         bool
}

// ContextPerSequence matches llama.cpp normalization with --no-kv-unified.
func ContextPerSequence(ctx, slots int) int {
	padded := (ctx + 255) / 256 * 256
	per := padded / slots
	return (per + 255) / 256 * 256
}

// llmTerms are the KV cache, compute buffer and slot state of a launch.
func llmTerms(shape LLMShape, l LLMLaunch) (kv, compute, slotState int64) {
	slots := max(1, l.Slots)
	if !l.Decision {
		kv = KVCacheBytes(shape.Geometry, l.CtxTokens, slots, l.CacheTypeK, l.CacheTypeV)
	}
	if l.Decision {
		ub := int64(ContextPerSequence(l.CtxTokens, slots))
		compute = ub * 4 * decisionLiveEmbdActivation * int64(shape.EmbeddingLen)
	} else {
		compute = llamaUbatchTokens * 4 * (int64(shape.VocabSize) + llamaLiveEmbdActivations*int64(shape.EmbeddingLen))
	}
	slotState = int64(slots) * (int64(shape.VocabSize)*llamaSlotBytesPerVocab + llamaSlotFixedBytes)
	return kv, compute, slotState
}

// EstimateLLM ports estimateLlmMemory (flash attention on).
func EstimateLLM(weights int64, shape LLMShape, l LLMLaunch) Demand {
	kv, compute, slotState := llmTerms(shape, l)
	conf := "derived"
	if !shape.FromGGUF {
		conf = "estimated"
	}
	return Demand{
		AcceleratorBytes: weights + kv + compute + AcceleratorOverhead,
		HostBytes:        weights + RuntimeHostOverhead + kv + l.PromptCacheBytes + slotState,
		UnifiedBytes:     weights + kv + compute + slotState + l.PromptCacheBytes + RuntimeHostOverhead,
		Confidence:       conf,
	}
}

// MeasuredDemand ports measuredDemand for LLM records.
func MeasuredDemand(r Measurement, artifactBytes, promptCache int64) Demand {
	return measuredDemand(r, artifactBytes, promptCache, AcceleratorOverhead, 0)
}

func measuredDemand(r Measurement, artifactBytes, promptCache, overhead, rounding int64) Demand {
	raw := r.BackendAcceleratorBytes + overhead
	acc := raw
	if rounding > 0 {
		acc = roundUp(raw, rounding)
	}
	host := max(r.PeakRSSBytes, r.HostBufferBytes, artifactBytes)
	return Demand{
		AcceleratorBytes: acc,
		HostBytes:        host + promptCache,
		UnifiedBytes:     max(raw+r.HostBufferBytes, host, r.SystemDeltaBytes) + promptCache,
		Confidence:       "measured",
	}
}

func findLLMMeasurement(id string, l LLMLaunch) *Measurement {
	m := byModel("llm", id)
	if m != nil && m.Shape.CtxTokens == l.CtxTokens && m.Shape.Slots == l.Slots && m.Shape.CacheTypeK == l.CacheTypeK && m.Shape.CacheTypeV == l.CacheTypeV {
		return m
	}
	return nil
}

// LaunchShape is the observed-memory key: `ctx=N;slots=N;kv=K/V`.
func (l LLMLaunch) LaunchShape() string {
	return "ctx=" + strconv.Itoa(l.CtxTokens) + ";slots=" + strconv.Itoa(l.Slots) + ";kv=" + l.CacheTypeK + "/" + l.CacheTypeV
}

// MeasuredLLMDemand returns the measured demand when this exact shape was measured.
func MeasuredLLMDemand(modelID string, l LLMLaunch, artifactBytes int64) (Demand, bool) {
	r := findLLMMeasurement(modelID, l)
	if r == nil {
		return Demand{}, false
	}
	return MeasuredDemand(*r, artifactBytes, l.PromptCacheBytes), true
}
