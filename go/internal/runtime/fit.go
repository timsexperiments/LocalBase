package runtime

import (
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Fit is the `memory` object of a /v1/models entry: this machine's minimum
// for the model, where the figure came from, and the verdict.
type Fit struct {
	MinVramGB         int    `json:"min_vram_gb"`
	Source            string `json:"source"`
	LaunchShape       string `json:"launch_shape"`
	ObservedPeakBytes *int64 `json:"observed_peak_bytes"`
	Fit               string `json:"fit"`
	SystemMemoryGB    int    `json:"system_memory_gb"`
}

// referenceLaunch is the catalog fit shape: 8K (or the model window), one
// slot, no prompt cache, and the KV type of the resolved shape. Unlike the
// launch plan, a missing file uses the size-based fallback geometry (q8_0
// compatible), as the TS catalogLlmReference does.
func referenceLaunch(spec *catalog.Spec, shape memory.LLMShape) memory.LLMLaunch {
	kv := "q8_0"
	if spec.IsEmbedding() || !shape.Geometry.Q8Compatible {
		kv = "f16"
	}
	ctx := memory.LLMReferenceContext
	if spec.ContextWindowTokens != nil {
		ctx = min(ctx, *spec.ContextWindowTokens)
	}
	return memory.LLMLaunch{CtxTokens: ctx, Slots: 1, CacheTypeK: kv, CacheTypeV: kv, Decision: spec.IsDecision()}
}

// FitFor ports evaluateModelFit for LLMs: observed (reference shape first,
// else newest) > measured > estimate. Other kinds return nil until their
// estimators are ported.
func FitFor(spec *catalog.Spec, systemGB int, g *memory.Geometry, observed []store.ObservedPeak) *Fit {
	if spec == nil || spec.Kind != "llm" {
		return nil
	}
	weights := spec.TotalArtifactBytes()
	resolved := memory.ResolveShape(spec.Size, spec.Quant, weights, g)
	l := referenceLaunch(spec, resolved)
	shape := l.LaunchShape()
	ref := memory.EstimateLLM(weights, resolved, l).AcceleratorBytes
	source := "estimate"
	if spec.Qualified() {
		if d, ok := memory.MeasuredLLMDemand(spec.ModelID, l, weights); ok {
			ref, source = d.AcceleratorBytes, "measured"
		}
	}
	f := &Fit{Source: source, LaunchShape: shape, SystemMemoryGB: systemGB}
	var usable []store.ObservedPeak
	for _, p := range observed {
		if p.Method == "footprint" && p.PeakBytes > 0 {
			usable = append(usable, p)
		}
	}
	demand := ref
	if len(usable) > 0 {
		pick := usable[0] // newest first
		for _, p := range usable {
			if p.LaunchShape == shape {
				pick = p
				break
			}
		}
		demand = pick.PeakBytes
		peak := pick.PeakBytes
		f.Source, f.LaunchShape, f.ObservedPeakBytes = "observed", pick.LaunchShape, &peak
	}
	f.MinVramGB = memory.MinimumMemoryGB(demand)
	switch {
	case systemGB < f.MinVramGB:
		f.Fit = "wont-fit"
	case systemGB-f.MinVramGB < 4 || (systemGB > 0 && float64(f.MinVramGB)/float64(systemGB) > 0.75):
		f.Fit = "tight"
	default:
		f.Fit = "fits"
	}
	return f
}
