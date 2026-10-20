// Package meta projects catalog facts and device state for one model: the
// /_localbase/models entries, `models show`, and the catalog query both
// the API and the CLI filter with (model-metadata.ts, catalog-query.ts). It
// never starts runtimes, downloads or hashes files.
package meta

import (
	"context"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// GET /_localbase/models and /_localbase/models/{id}: catalog facts and
// observed device state (model-metadata.ts). It never starts runtimes,
// downloads or hashes files.

type Artifact struct {
	Role      string          `json:"role"`
	Filename  string          `json:"filename"`
	Source    *artifactSource `json:"source"`
	SHA256    *string         `json:"sha256"`
	SizeBytes *int64          `json:"sizeBytes"`
}

type artifactSource struct {
	RepositoryURL string `json:"repositoryUrl"`
	Revision      string `json:"revision"`
}

type Peak struct {
	PeakBytes   int64  `json:"peakBytes"`
	LaunchShape string `json:"launchShape"`
	Method      string `json:"method"`
	ObservedAt  string `json:"observedAt"`
}

type Memory struct {
	MinimumVramEstimateGb int     `json:"minimumVramEstimateGb"`
	StorageEstimateGb     float64 `json:"storageEstimateGb"`
	Source                string  `json:"source"`
	DemandBytes           int64   `json:"demandBytes"`
	LaunchShape           string  `json:"launchShape"`
	Reference             struct {
		Source      string `json:"source"`
		Bytes       int64  `json:"bytes"`
		LaunchShape string `json:"launchShape"`
	} `json:"reference"`
	ObservedPeaks []Peak `json:"observedPeaks"`
	Observed      *Peak  `json:"observed"`
	FeedPeak      any    `json:"feedPeak"`
	Fit           struct {
		Verdict        string  `json:"verdict"`
		SystemMemoryGb int     `json:"systemMemoryGb"`
		HeadroomGb     float64 `json:"headroomGb"`
	} `json:"fit"`
}

type CapabilityResolution struct {
	Capabilities    []catalog.Capability `json:"capabilities"`
	Mismatches      []runtime.Mismatch   `json:"mismatches"`
	InputModalities struct {
		Values []string `json:"values"`
		Source string   `json:"source"`
	} `json:"inputModalities"`
}

type CatalogInfo struct {
	Name                    string                `json:"name"`
	DisplayName             string                `json:"displayName"`
	Provider                string                `json:"provider"`
	Source                  string                `json:"source"`
	Revision                string                `json:"revision"`
	Kind                    string                `json:"kind"`
	QualificationState      string                `json:"qualificationState"`
	Trust                   string                `json:"trust"`
	CatalogTier             string                `json:"catalogTier"`
	CatalogFeed             *string               `json:"catalogFeed"`
	CommercialStatus        string                `json:"commercialStatus"`
	Summary                 string                `json:"summary"`
	Notes                   string                `json:"notes"`
	Quantization            string                `json:"quantization"`
	Features                []string              `json:"features"`
	Artifacts               []Artifact            `json:"artifacts"`
	Memory                  Memory                `json:"memory"`
	Capabilities            any                   `json:"capabilities"`
	InputModalities         []string              `json:"inputModalities"`
	OutputModalities        []string              `json:"outputModalities"`
	ContextWindowTokens     *int                  `json:"contextWindowTokens"`
	MaxOutputTokens         any                   `json:"maxOutputTokens"`
	UnsupportedCapabilities []catalog.Unsupported `json:"unsupportedCapabilities"`
	FeedAnnotation          any                   `json:"feedAnnotation"`
	CapabilityResolution    CapabilityResolution  `json:"capabilityResolution"`
}

type Runtime struct {
	Configured                 bool   `json:"configured"`
	State                      string `json:"state"`
	ExecutionSlots             *int   `json:"executionSlots"`
	ActiveAdmissions           *int   `json:"activeAdmissions"`
	AvailableExecutionSlots    *int   `json:"availableExecutionSlots"`
	ImmediateDispatchAvailable *bool  `json:"immediateDispatchAvailable"`
	QueuedRequests             *int   `json:"queuedRequests"`
	WaitingCapacity            *int   `json:"waitingCapacity"`
	AvailableWaitingCapacity   *int   `json:"availableWaitingCapacity"`
}

type Model struct {
	Object  string      `json:"object"`
	ID      string      `json:"id"`
	Catalog CatalogInfo `json:"catalog"`
	Device  struct {
		Selected  bool     `json:"selected"`
		Installed bool     `json:"installed"`
		Runtime   *Runtime `json:"runtime"`
	} `json:"device"`
}

// features is the legacy `features` tag list.
// Features is the legacy `features` tag list.
func Features(s *catalog.Spec) []string {
	tags := []string{}
	for _, c := range s.Capabilities {
		tags = append(tags, c.ID)
	}
	if s.OutputFormat != "" {
		tags = append(tags, s.OutputFormat+"-output")
	}
	if s.Kind == "tts" {
		tags = append(tags, "cold-per-request")
	}
	if s.TTSRuntime != nil && s.TTSRuntime.Kind == "llama-tts" {
		tags = append(tags, "catalog-reference-voices")
	}
	if s.TTSRuntime != nil && s.TTSRuntime.Kind == "kokoro-onnx" {
		tags = append(tags, "bundled-native-voices")
	}
	if s.ExperimentalRuntime {
		tags = append(tags, "experimental")
	}
	if s.VideoCatalogOnly {
		tags = append(tags, "catalog-only")
	}
	if s.DiffusionWeightsOnly {
		tags = append(tags, "diffusion-weights-only")
	}
	return tags
}

const imageLoraMaxStrength = 2

// kindCapabilities is the kind-specific `capabilities` object, or nil.
func kindCapabilities(s *catalog.Spec, voices []store.Voice) any {
	switch {
	case s.IsEmbedding():
		var d struct {
			Minimum int `json:"minimum"`
			Maximum int `json:"maximum"`
		}
		jsonUnmarshal(s.LLMRuntime.Dimensions, &d)
		return map[string]any{"kind": "embedding", "dimensions": d}
	case s.IsDecision():
		return map[string]any{"kind": "decision"}
	case s.Kind == "tts" && s.TTSRuntime != nil && s.TTSRuntime.Kind == "llama-tts":
		values := []string{"default"}
		refs := []map[string]any{}
		for _, v := range s.TTSRuntime.ReferenceVoices {
			values = append(values, v.Name)
			refs = append(refs, map[string]any{"name": v.Name, "license": v.License, "provenanceUrl": v.ProvenanceURL})
		}
		custom := []map[string]any{}
		for _, v := range voices {
			values = append(values, v.ID)
			custom = append(custom, map[string]any{"requestValue": v.ID, "name": v.Name})
		}
		return map[string]any{"kind": "speech", "outputFormats": []string{"wav"}, "voice": map[string]any{
			"selection": "catalog-reference", "requestValues": values, "defaultRequestValue": "default",
			"references": refs, "custom": custom}, "residency": "cold-per-request"}
	case s.Kind == "tts" && s.TTSRuntime != nil && s.TTSRuntime.Kind == "kokoro-onnx":
		values := []string{}
		for _, v := range s.TTSRuntime.Voices {
			values = append(values, v.Name)
		}
		return map[string]any{"kind": "speech", "outputFormats": []string{"wav"}, "voice": map[string]any{
			"selection": "bundled-native", "requestValues": values, "defaultRequestValue": s.TTSRuntime.DefaultVoice},
			"residency": "cold-per-request"}
	case s.VideoRuntime != nil:
		q := s.VideoRuntime.Qualification
		return map[string]any{"kind": "video", "mode": s.VideoRuntime.Mode, "width": q.MaxWidth, "height": q.MaxHeight,
			"frames": q.MaxFrames, "fps": q.FPS, "jobDeadlineMs": q.JobDeadlineMs, "outputFormats": []string{"mp4"}}
	case s.ImageLaunchOptions != nil && s.ImageLaunchOptions.Lora != nil:
		l := s.ImageLaunchOptions.Lora
		var trigger any
		if l.Trigger != "" {
			trigger = l.Trigger
		}
		return map[string]any{"kind": "image", "lora": map[string]any{"defaultStrength": l.DefaultStrength, "minimum": 0,
			"maximum": imageLoraMaxStrength, "trigger": trigger}}
	}
	return nil
}

func tierOf(s *catalog.Spec) string {
	if s.Trust == "qualified" {
		return "built-in"
	}
	return "user"
}

// Input is what a projection reads besides the spec.
type Input struct {
	Store    *store.Store
	RootDir  string
	Config   *store.Config
	MemoryGB int
	Voices   []store.Voice
	// Snapshot is the LLM runtime state (gateway only).
	Snapshot *runtime.Snapshot
	// CLI uses observed peaks without the installation rule or geometry, as
	// TS cliMemoryInputs does.
	CLI bool
}

// Project builds one model's metadata.
func Project(ctx context.Context, in Input, spec *catalog.Spec) Model {
	cfg := in.Config
	dir := filepath.Join(in.RootDir, "models", spec.Kind)
	installed, _ := spec.Installation(dir)
	var fit *runtime.Fit
	if in.CLI {
		var peaks []store.ObservedPeak
		if sha := spec.Primary().SHA256; sha != "" && in.Store != nil {
			peaks, _ = in.Store.ObservedPeaks(ctx, spec.ModelID, sha)
		}
		fit = runtime.FitFor(spec, in.MemoryGB, nil, peaks)
	} else {
		fit = runtime.ModelFit(ctx, in.Store, spec, dir, in.MemoryGB)
	}
	res := runtime.CapabilitiesFor(ctx, in.Store, spec, dir)
	voices := in.Voices

	m := Model{Object: "localbase.model", ID: spec.ModelID}
	c := &m.Catalog
	c.Name, c.DisplayName, c.Provider, c.Source = spec.Family, spec.Name(), spec.Provider, spec.Source
	c.Revision, c.Kind, c.QualificationState = spec.RepositoryRevision, spec.Kind, spec.QualificationState
	c.Trust, c.CatalogTier = spec.Trust, tierOf(spec)
	c.CommercialStatus, c.Summary, c.Notes, c.Quantization = spec.CommercialStatus, spec.Catch, spec.Notes, spec.Quant
	c.Features = Features(spec)
	for _, a := range spec.Artifacts {
		ma := Artifact{Role: a.Role, Filename: a.Filename}
		if len(a.Source) > 0 {
			var src artifactSource
			jsonUnmarshal(a.Source, &src)
			ma.Source = &src
		}
		if a.SHA256 != "" {
			sha := a.SHA256
			ma.SHA256 = &sha
		}
		if a.ExpectedSizeBytes > 0 {
			size := a.ExpectedSizeBytes
			ma.SizeBytes = &size
		}
		c.Artifacts = append(c.Artifacts, ma)
	}
	mem := &c.Memory
	mem.MinimumVramEstimateGb, mem.StorageEstimateGb = fit.MinVramGB, spec.StorageGB
	mem.Source, mem.DemandBytes, mem.LaunchShape = fit.Source, fit.DemandBytes, fit.LaunchShape
	mem.Reference.Source, mem.Reference.Bytes, mem.Reference.LaunchShape = fit.ReferenceSource, fit.ReferenceBytes, fit.ReferenceShape
	mem.ObservedPeaks = []Peak{}
	for _, p := range fit.Peaks {
		mem.ObservedPeaks = append(mem.ObservedPeaks, Peak{p.PeakBytes, p.LaunchShape, p.Method, p.ObservedAt})
	}
	if o := fit.Observed; o != nil {
		mem.Observed = &Peak{o.PeakBytes, o.LaunchShape, o.Method, o.ObservedAt}
	}
	mem.Fit.Verdict, mem.Fit.SystemMemoryGb = fit.Fit, fit.SystemMemoryGB
	mem.Fit.HeadroomGb = float64(fit.SystemMemoryGB - fit.MinVramGB)
	c.Capabilities = kindCapabilities(spec, voices)
	c.InputModalities, c.OutputModalities = spec.InputModalities, spec.OutputModalities
	c.ContextWindowTokens = spec.ContextWindowTokens
	c.UnsupportedCapabilities = spec.UnsupportedCapabilities
	if c.UnsupportedCapabilities == nil {
		c.UnsupportedCapabilities = []catalog.Unsupported{}
	}
	c.CapabilityResolution.Capabilities = res.Capabilities
	if res.Capabilities == nil {
		c.CapabilityResolution.Capabilities = []catalog.Capability{}
	}
	c.CapabilityResolution.Mismatches = res.Mismatches
	if res.Mismatches == nil {
		c.CapabilityResolution.Mismatches = []runtime.Mismatch{}
	}
	c.CapabilityResolution.InputModalities.Values = res.InputModalities
	c.CapabilityResolution.InputModalities.Source = res.InputSource

	selected := false
	for _, kind := range store.Kinds {
		if slices.Contains(cfg.Selected[kind], spec.ModelID) {
			selected = true
		}
	}
	m.Device.Selected, m.Device.Installed = selected, installed
	if in.Snapshot != nil && spec.Kind == "llm" && in.Snapshot.ModelID == spec.ModelID {
		m.Device.Runtime = runtimeView(*in.Snapshot)
	}
	return m
}

func runtimeView(snap runtime.Snapshot) *Runtime {
	r := &Runtime{Configured: snap.State != "disabled", State: snap.State}
	waiting, capacity := snap.Waiting, snap.Capacity
	r.QueuedRequests, r.WaitingCapacity = &waiting, &capacity
	avail := 0
	if snap.Accepting {
		avail = max(0, capacity-waiting)
	}
	r.AvailableWaitingCapacity = &avail
	active := snap.Active
	r.ActiveAdmissions = &active
	if snap.Slots > 0 {
		slots, free := snap.Slots, max(0, snap.Slots-snap.Active)
		r.ExecutionSlots, r.AvailableExecutionSlots = &slots, &free
		immediate := snap.Accepting && snap.State == "running" && free > 0 && waiting == 0
		r.ImmediateDispatchAvailable = &immediate
	}
	return r
}

var (
	fitRank      = map[string]int{"fits": 0, "tight": 1, "wont-fit": 2}
	sortKeys     = []string{"name", "size", "memory", "fit", "context"}
	licenseNames = []string{"open", "conditional", "prohibited"}
)

func listParam(values []string, allowed func(string) bool) []string {
	var out []string
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" && allowed(part) {
				out = append(out, part)
			}
		}
	}
	return out
}

// Query filters and sorts by the optional q, kind, capability, fit,
// installed, license, sort and reverse parameters; unknown values are
// dropped, and without a sort the catalog order is kept.
func Query(data []Model, q url.Values) []Model {
	return QueryWithText(data, q, func(m Model) []string {
		return []string{m.Catalog.Name, m.Catalog.Quantization, m.Catalog.Summary}
	})
}

// QueryWithText is Query with the free text search matches besides the id
// and display name.
func QueryWithText(data []Model, q url.Values, text func(Model) []string) []Model {
	search := strings.Fields(strings.ToLower(strings.TrimSpace(q.Get("q"))))
	kinds := listParam(q["kind"], func(k string) bool { return slices.Contains(store.Kinds, k) })
	caps := listParam(q["capability"], func(string) bool { return true })
	fits := listParam(q["fit"], func(f string) bool { _, ok := fitRank[f]; return ok })
	licenses := listParam(q["license"], func(l string) bool { return slices.Contains(licenseNames, l) })
	installed := q.Get("installed")
	var out []Model
	for _, m := range data {
		c := m.Catalog
		effective := map[string]bool{}
		for _, cap := range c.CapabilityResolution.Capabilities {
			effective[cap.ID] = true
		}
		switch {
		case len(kinds) > 0 && !slices.Contains(kinds, c.Kind):
			continue
		case (installed == "true" || installed == "false") && m.Device.Installed != (installed == "true"):
			continue
		case len(fits) > 0 && !slices.Contains(fits, c.Memory.Fit.Verdict):
			continue
		case len(licenses) > 0 && !slices.Contains(licenses, c.CommercialStatus):
			continue
		}
		missing := false
		for _, cap := range caps {
			if !effective[cap] {
				missing = true
			}
		}
		if missing {
			continue
		}
		hay := strings.ToLower(strings.Join(append([]string{m.ID, c.DisplayName}, text(m)...), "\n"))
		match := true
		for _, t := range search {
			if !strings.Contains(hay, t) {
				match = false
			}
		}
		if match {
			out = append(out, m)
		}
	}
	sortKey := q.Get("sort")
	reverse := q.Get("reverse") == "true"
	if !slices.Contains(sortKeys, sortKey) {
		if reverse {
			slices.Reverse(out)
		}
		return out
	}
	name := func(m Model) string { return m.Catalog.DisplayName }
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		missA := sortKey == "context" && a.Catalog.ContextWindowTokens == nil
		missB := sortKey == "context" && b.Catalog.ContextWindowTokens == nil
		if missA != missB {
			return missB
		}
		cmp := 0.0
		if !missA {
			switch sortKey {
			case "name":
				cmp = float64(caseFold(name(a), name(b)))
			case "size":
				cmp = a.Catalog.Memory.StorageEstimateGb - b.Catalog.Memory.StorageEstimateGb
			case "memory":
				cmp = float64(a.Catalog.Memory.MinimumVramEstimateGb - b.Catalog.Memory.MinimumVramEstimateGb)
			case "fit":
				cmp = float64(fitRank[a.Catalog.Memory.Fit.Verdict] - fitRank[b.Catalog.Memory.Fit.Verdict])
				if cmp == 0 {
					cmp = float64(a.Catalog.Memory.MinimumVramEstimateGb - b.Catalog.Memory.MinimumVramEstimateGb)
				}
			case "context":
				cmp = float64(*b.Catalog.ContextWindowTokens - *a.Catalog.ContextWindowTokens)
			}
		}
		if cmp == 0 {
			cmp = float64(localeCompare(name(a), name(b)))
		}
		if cmp == 0 {
			cmp = float64(localeCompare(a.ID, b.ID))
		}
		if reverse {
			cmp = -cmp
		}
		return cmp < 0
	})
	return out
}
