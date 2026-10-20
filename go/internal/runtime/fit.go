package runtime

import (
	"context"
	"path/filepath"
	"strings"

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

	// Detail for /_localbase/models.
	DemandBytes     int64                `json:"-"`
	ReferenceBytes  int64                `json:"-"`
	ReferenceSource string               `json:"-"`
	ReferenceShape  string               `json:"-"`
	Observed        *store.ObservedPeak  `json:"-"`
	Peaks           []store.ObservedPeak `json:"-"`
}

// ReferenceShape is the launch-shape key of a model's catalog reference
// launch: the LLM shape, or "default" for the other kinds.
func ReferenceShape(spec *catalog.Spec, g *memory.Geometry) string {
	if spec.Kind != "llm" {
		return "default"
	}
	_, _, l := memory.LLMReference(spec, g)
	return l.LaunchShape()
}

// FitFor ports evaluateModelFit: observed (reference shape first, else
// newest) > measured > estimate. Feed peaks arrive with the feed (M5). g is
// the installed GGUF geometry (LLM or TTS talker), or nil.
func FitFor(spec *catalog.Spec, systemGB int, g *memory.Geometry, observed []store.ObservedPeak) *Fit {
	if spec == nil {
		return nil
	}
	ref, source := memory.EstimateCatalog(spec, g).AcceleratorBytes, "estimate"
	if d, ok := memory.MeasuredCatalog(spec, g); ok {
		ref, source = d.AcceleratorBytes, "measured"
	}
	shape := ReferenceShape(spec, g)
	f := &Fit{Source: source, LaunchShape: shape, SystemMemoryGB: systemGB, ReferenceBytes: ref, ReferenceSource: source,
		ReferenceShape: shape, Peaks: observed}
	demand := ref
	var pick *store.ObservedPeak
	for i := range observed { // newest first
		p := &observed[i]
		if p.Method != "footprint" || p.PeakBytes <= 0 {
			continue
		}
		if pick == nil {
			pick = p
		}
		if p.LaunchShape == shape {
			pick = p
			break
		}
	}
	if pick != nil {
		demand = pick.PeakBytes
		peak := pick.PeakBytes
		f.Source, f.LaunchShape, f.ObservedPeakBytes, f.Observed = "observed", pick.LaunchShape, &peak, pick
	}
	f.DemandBytes = demand
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

// ModelFit is a model's memory figure on this machine. Like TS, local facts
// (GGUF geometry, observed peaks) count only once every artifact in dir is
// installed.
func ModelFit(ctx context.Context, st *store.Store, spec *catalog.Spec, dir string, systemGB int) *Fit {
	if complete, _ := spec.Installation(dir); !complete {
		return FitFor(spec, systemGB, nil, nil)
	}
	var geo *memory.Geometry
	primary := spec.Primary()
	if (spec.Kind == "llm" || (spec.TTSRuntime != nil && spec.TTSRuntime.Kind == "llama-tts")) && strings.HasSuffix(primary.Filename, ".gguf") {
		geo = memory.ReadGeometry(filepath.Join(dir, primary.Filename))
	}
	var peaks []store.ObservedPeak
	if primary.SHA256 != "" && st != nil {
		peaks, _ = st.ObservedPeaks(ctx, spec.ModelID, primary.SHA256)
	}
	return FitFor(spec, systemGB, geo, peaks)
}
