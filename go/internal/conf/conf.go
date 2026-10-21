// Package conf holds configuration rules that need the catalog: defaults,
// model-selection validation, and the public JSON shape of the config.
package conf

import (
	"fmt"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"sort"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

const (
	DefaultGatewayPort = 2273
	DefaultLLMPort     = 18000
	DefaultSTTPort     = 18080
	DefaultCtxSize     = 131072
	defaultLLM         = "qwen2.5-coder-7b-instruct-q4_k_m"
	defaultImage       = "stable-diffusion-v1-5"
)

// Default is a fresh configuration for root (TS defaultConfig). The STT
// default is the third smallest STT model that fits memoryGB.
func Default(root string, memoryGB int, cat *catalog.Catalog) *store.Config {
	stt := "whisper-base-q8_0"
	var fitting []*catalog.Spec
	for _, s := range cat.All() {
		if s.Kind == "stt" && runtime.FitFor(s, 0, nil, nil, cat.PlannableFeedPeaks(s)).MinVramGB <= memoryGB {
			fitting = append(fitting, s)
		}
	}
	sort.SliceStable(fitting, func(i, j int) bool { return fitting[i].StorageGB < fitting[j].StorageGB })
	if len(fitting) > 2 {
		stt = fitting[2].ModelID
	} else if len(fitting) > 0 {
		stt = fitting[0].ModelID
	}
	return &store.Config{
		Root: root, GatewayHost: "127.0.0.1", GatewayPort: DefaultGatewayPort, Host: "127.0.0.1", Port: DefaultLLMPort,
		CtxSize: DefaultCtxSize, CtxMode: "auto", SttHost: "127.0.0.1", SttPort: DefaultSTTPort,
		Selected: map[string][]string{"llm": {defaultLLM}, "stt": {stt}, "tts": {}, "image": {defaultImage}, "video": {}},
		Active:   map[string]string{"llm": defaultLLM, "stt": stt, "tts": "", "image": defaultImage, "video": ""},
		Parallel: "auto", OtelSampleRatio: 100,
		SystemReserve:      store.MemoryReserve{Percent: 15, MinimumGB: 8},
		AcceleratorReserve: store.MemoryReserve{Percent: 10, MinimumGB: 2},
	}
}

var expectedModalities = map[string][2]string{"llm": {"text", "text"}, "stt": {"audio", "text"},
	"tts": {"text", "audio"}, "image": {"text", "image"}, "video": {"text", "video"}}

const (
	ExperimentalOptIn   = "Experimental models require models.allowExperimental = true."
	UnsupportedTarget   = "Requires Linux x64 with a single NVIDIA GPU."
	selectionKindFields = "selected%sModels"
)

// VideoTarget is the host's video runtime target, or "" when the host has
// none (TS videoTargetFromTopology). Linux NVIDIA targets are deferred (§11).
func VideoTarget() string {
	if goruntime.GOOS == "darwin" && goruntime.GOARCH == "arm64" {
		return "darwin/arm64/apple-unified"
	}
	return ""
}

// Ineligible returns why a model cannot be selected on this host, or "".
func Ineligible(s *catalog.Spec, allowExperimental bool) string {
	if s.QualificationState == "experimental" && !allowExperimental {
		return ExperimentalOptIn
	}
	if s.VideoRuntime == nil {
		return ""
	}
	target := VideoTarget()
	for _, t := range s.VideoRuntime.SupportedTargets {
		if t.Platform+"/"+t.Architecture+"/"+t.Accelerator == target {
			return ""
		}
	}
	return UnsupportedTarget
}

var fieldNames = map[string]string{"llm": "Llm", "stt": "Stt", "tts": "Tts", "image": "Image", "video": "Video"}

// Issue is one validation failure, formatted like TS issue summaries.
type Issue struct{ Path, Message string }

func (i Issue) String() string { return i.Path + ": " + i.Message }

// ValidateModels checks selections and active models against the catalog.
// eligibility also applies the experimental and platform rules (saving);
// loading skips them, as TS does.
func ValidateModels(c *store.Config, cat *catalog.Catalog, eligibility bool) []Issue {
	var issues []Issue
	check := func(field, id, kind string) {
		s := cat.ByID(id)
		exp := expectedModalities[kind]
		switch {
		case s == nil || s.Kind != kind || !slices.Contains(s.InputModalities, exp[0]) || !slices.Contains(s.OutputModalities, exp[1]):
			issues = append(issues, Issue{field, "must name a catalog " + kind + " model with compatible modalities"})
		case s.VideoCatalogOnly:
			issues = append(issues, Issue{field, "catalog-only models cannot be selected or active because LocalBase inference is unavailable"})
		case eligibility && Ineligible(s, c.AllowExperimental) != "":
			issues = append(issues, Issue{field, "model is experimental or unsupported on this platform"},
				Issue{field, Ineligible(s, c.AllowExperimental)})
		}
	}
	for _, kind := range store.Kinds {
		field := fmt.Sprintf(selectionKindFields, fieldNames[kind])
		ids := c.Selected[kind]
		if kind == "llm" && len(ids) == 0 {
			issues = append(issues, Issue{field, "Too small: expected array to have >=1 items"})
		}
		seen := map[string]bool{}
		for i, id := range ids {
			if seen[id] {
				issues = append(issues, Issue{field, "must not contain duplicates"})
			}
			seen[id] = true
			check(fmt.Sprintf("%s.%d", field, i), id, kind)
		}
		activeField := "active" + fieldNames[kind] + "Model"
		if a := c.Active[kind]; a != "" {
			check(activeField, a, kind)
			if !slices.Contains(ids, a) {
				issues = append(issues, Issue{activeField, "must also be present in its selected model list"})
			}
		} else if kind == "llm" {
			issues = append(issues, Issue{activeField, "Too small: expected string to have >=1 characters"})
		}
	}
	return issues
}

// ValidateRow checks the non-model fields.
func ValidateRow(c *store.Config) []Issue {
	var issues []Issue
	host := func(field, h string) {
		if h == "" || len(h) > 253 || strings.ContainsAny(h, " \t\r\n") {
			issues = append(issues, Issue{field, "must be a host name without whitespace"})
		}
	}
	port := func(field string, p int) {
		if p < 1 || p > 65535 {
			issues = append(issues, Issue{field, "must be a port from 1 to 65535"})
		}
	}
	if !filepath.IsAbs(c.Root) {
		issues = append(issues, Issue{"root", "must be an absolute path"})
	}
	host("gatewayHost", c.GatewayHost)
	port("gatewayPort", c.GatewayPort)
	host("host", c.Host)
	port("port", c.Port)
	host("sttHost", c.SttHost)
	port("sttPort", c.SttPort)
	if c.CtxSize < 2048 || c.CtxSize > 2_147_483_647 {
		issues = append(issues, Issue{"ctxSize", "must be at least 2048"})
	}
	if c.CtxMode != "auto" && c.CtxMode != "explicit" {
		issues = append(issues, Issue{"ctxMode", `must be "auto" or "explicit"`})
	}
	if !slices.Contains([]string{"auto", "1", "2", "3", "4"}, c.Parallel) {
		issues = append(issues, Issue{"parallel", `must be "auto" or 1-4`})
	}
	if c.OtelEndpoint != "" {
		if msg := obs.ValidateEndpoint(c.OtelEndpoint); msg != "" {
			issues = append(issues, Issue{"otelEndpoint", msg})
		}
	}
	if _, err := obs.ParseHeaders(c.OtelHeaders); err != nil {
		issues = append(issues, Issue{"otelHeaders", err.Error()})
	}
	if c.OtelSampleRatio < 0 || c.OtelSampleRatio > 100 {
		issues = append(issues, Issue{"otelSampleRatio", "must be from 0 to 100"})
	}
	for name, r := range map[string]store.MemoryReserve{"memory.systemReserve": c.SystemReserve, "memory.acceleratorReserve": c.AcceleratorReserve} {
		if r.Percent < 0 || r.Percent > 100 || r.MinimumGB < 0 {
			issues = append(issues, Issue{name, "must be a percent from 0 to 100 and a non-negative minimum"})
		}
	}
	return issues
}

// Summary formats issues as TS issueSummary does.
func Summary(issues []Issue) string {
	parts := make([]string, len(issues))
	for i, is := range issues {
		parts[i] = is.String()
	}
	return strings.Join(parts, "; ")
}

// InvalidError is an invalid persisted or proposed configuration.
type InvalidError struct {
	Root   string
	Issues []Issue
}

func (e *InvalidError) Error() string {
	return fmt.Sprintf("Invalid LocalBase configuration in %s: %s. Repair the invalid row or run `local-base configure`.",
		filepath.Join(e.Root, "local-base.db"), Summary(e.Issues))
}

// Check validates a config for saving (row rules, models, eligibility).
func Check(c *store.Config, cat *catalog.Catalog) error {
	issues := append(ValidateRow(c), ValidateModels(c, cat, true)...)
	if len(issues) > 0 {
		return &InvalidError{c.Root, issues}
	}
	return nil
}

// Public is the configuration as commands print it (secrets omitted).
func Public(c *store.Config) map[string]any {
	dirs := map[string]string{}
	for _, k := range store.Kinds {
		dirs[k] = filepath.Join(c.Root, "models", k)
	}
	var parallel any = c.Parallel
	if c.Parallel != "auto" {
		parallel = int(c.Parallel[0] - '0')
	}
	endpoint := obs.SanitizedEndpoint(c.OtelEndpoint)
	source := "disabled"
	if c.OtelEndpoint != "" {
		source = "persistent"
	}
	return map[string]any{
		"root": c.Root, "llmModelsDir": dirs["llm"], "sttModelsDir": dirs["stt"], "ttsModelsDir": dirs["tts"],
		"imageModelsDir": dirs["image"], "videoModelsDir": dirs["video"], "host": c.Host, "port": c.Port,
		"ctxSize": c.CtxSize, "ctxMode": c.CtxMode, "sttHost": c.SttHost, "sttPort": c.SttPort,
		"selectedLlmModels": c.Selected["llm"], "selectedSttModels": c.Selected["stt"],
		"selectedTtsModels": c.Selected["tts"], "selectedImageModels": c.Selected["image"],
		"selectedVideoModels": c.Selected["video"], "activeLlmModel": c.Active["llm"], "activeSttModel": c.Active["stt"],
		"activeTtsModel": c.Active["tts"], "activeImageModel": c.Active["image"], "activeVideoModel": c.Active["video"],
		"parallel":          parallel,
		"memory":            map[string]any{"systemReserve": c.SystemReserve, "acceleratorReserve": c.AcceleratorReserve},
		"hfTokenConfigured": c.HFToken != "",
		"observability": map[string]any{"enabled": c.OtelEndpoint != "", "source": source, "endpoint": endpoint,
			"persistedEndpoint": endpoint, "sampler": "parentbased_traceidratio", "sampleRatio": float64(c.OtelSampleRatio) / 100},
	}
}
