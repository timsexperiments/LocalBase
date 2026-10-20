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
	layers := int(math.Ceil(16 * math.Pow(p, 0.4)))
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

// EstimateLLM ports estimateLlmMemory (flash attention on).
func EstimateLLM(weights int64, shape LLMShape, l LLMLaunch) Demand {
	slots := max(1, l.Slots)
	var kv int64
	if !l.Decision {
		kv = KVCacheBytes(shape.Geometry, l.CtxTokens, slots, l.CacheTypeK, l.CacheTypeV)
	}
	var compute int64
	if l.Decision {
		ub := int64(ContextPerSequence(l.CtxTokens, slots))
		compute = ub * 4 * decisionLiveEmbdActivation * int64(shape.EmbeddingLen)
	} else {
		compute = llamaUbatchTokens * 4 * (int64(shape.VocabSize) + llamaLiveEmbdActivations*int64(shape.EmbeddingLen))
	}
	slotState := int64(slots) * (int64(shape.VocabSize)*llamaSlotBytesPerVocab + llamaSlotFixedBytes)
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

// Measurement is a checked-in calibration record (memory-measurements.ts).
type Measurement struct {
	ModelID                 string
	CtxTokens, Slots        int
	CacheTypeK, CacheTypeV  string
	BackendAcceleratorBytes int64
	HostBufferBytes         int64
	PeakRSSBytes            int64
	SystemDeltaBytes        int64
}

func mib(v float64) int64 { return int64(math.Round(v * float64(MiB))) }

func llmRecord(id string, metalMiB, hostMiB float64, rss int64) Measurement {
	return Measurement{ModelID: id, CtxTokens: 8192, Slots: 1, CacheTypeK: "q8_0", CacheTypeV: "q8_0",
		BackendAcceleratorBytes: mib(metalMiB), HostBufferBytes: mib(hostMiB), PeakRSSBytes: rss}
}

// LLMMeasurements are the LLM records at the reference shape (2026-10-13..15).
var LLMMeasurements = []Measurement{
	llmRecord("qwen3.5-0.8b-q4_k_m", 521+70+497, 211, 827_424_768),
	llmRecord("qwen3.5-2b-q4_k_m", 1259+70+493, 414, 1_593_032_704),
	llmRecord("qwen3.5-4b-q4_k_m", 2740+186+495, 515, 3_320_250_368),
	llmRecord("qwen3.5-9b-q4_k_m", 5626+186+501, 569, 6_390_480_896),
	llmRecord("clef-flash-q4_k_m", 5630+1843+595+248, 1212, 8_551_874_560),
	llmRecord("qwen3.5-27b-q4_k_m", 16710+421+513, 710, 19_027_968_000),
	llmRecord("ministral-3-14b-instruct-q4_k_m", 7849+680+266, 388, 7_823_081_472),
	llmRecord("mistral-nemo-12b-instruct-q4_k_m", 8107, 388, 8_069_824_512),
	llmRecord("gpt-oss-20b-q4_k_m", 11120+111+404, 399, 10_298_408_960),
}

// MeasuredDemand ports measuredDemand for LLM records.
func MeasuredDemand(r Measurement, artifactBytes, promptCache int64) Demand {
	acc := r.BackendAcceleratorBytes + AcceleratorOverhead
	host := max(r.PeakRSSBytes, r.HostBufferBytes, artifactBytes)
	return Demand{
		AcceleratorBytes: acc,
		HostBytes:        host + promptCache,
		UnifiedBytes:     max(acc+r.HostBufferBytes, host, r.SystemDeltaBytes) + promptCache,
		Confidence:       "measured",
	}
}

func findLLMMeasurement(id string, l LLMLaunch) *Measurement {
	for i := range LLMMeasurements {
		m := &LLMMeasurements[i]
		if m.ModelID == id {
			if m.CtxTokens == l.CtxTokens && m.Slots == l.Slots && m.CacheTypeK == l.CacheTypeK && m.CacheTypeV == l.CacheTypeV {
				return m
			}
			return nil
		}
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
