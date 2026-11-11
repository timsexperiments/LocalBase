package conf

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Input is what configure was given on the command line, apart from the --config file: each option
// as the person typed it (nil when not given; a model list split, and present when given), and what
// the merge needs to know of the root and the machine. Because it is the command line's, MergeInput's
// refusals and warnings name its flags (--hf-token, --otel-headers).
type Input struct {
	Models      map[string][]string // --<kind>-models
	Active      map[string]*string  // --active-<kind>
	DefaultLLMs *[]string           // --default-llm-models

	Host, SttHost, CtxMode, OtelEndpoint, OtelHeaders, Parallel, HFToken *string
	Port, SttPort, CtxSize, OtelSampleRatio                              *string // parsed by the merge

	ExistingRoot bool   // the root had a database before this command
	HFTokenEnv   string // HF_TOKEN, which a new root takes when nothing else gives a token
	MemoryGB     int    // this machine's memory, for the parallel warning
}

// MergeInput merges what configure was given (in) and the --config file (overrides, nil for
// none) into cfg: an option given wins over the file, and the file over what is saved. It checks
// each option as it goes and stops at the first that isn't valid, with a *DocumentError
// (bad input). warnings are what to tell the person, in order, including on a refusal.
func MergeInput(cfg *store.Config, in Input, overrides *Overrides, cat *catalog.Catalog) (warnings []string, err error) {
	if overrides == nil {
		overrides = &Overrides{}
	}

	lists := map[string][]string{}
	for _, k := range store.Kinds {
		ids, given := in.Models[k]
		if !given {
			continue
		}
		if k == "llm" && len(ids) == 0 {
			return warnings, &DocumentError{Message: "llmModels: Too small: expected array to have >=1 items"}
		}
		probe := cfg.Clone()
		probe.Selected = map[string][]string{k: ids}
		probe.Active = map[string]string{}
		for _, is := range ValidateModels(probe, cat, k != "video") {
			if strings.HasPrefix(is.Path, "selected"+KindField(k)+"Models") {
				return warnings, &DocumentError{Message: is.String()}
			}
		}
		lists[k] = ids
	}
	for _, k := range store.Kinds {
		if ids, ok := lists[k]; ok {
			cfg.Selected[k] = ids
		} else if ids := overrides.Selected[k]; ids != nil {
			cfg.Selected[k] = ids
		}
		switch {
		case in.Active[k] != nil:
			if *in.Active[k] == "" {
				return warnings, &DocumentError{Message: fmt.Sprintf("active%s: Too small: expected string to have >=1 characters", KindField(k))}
			}
			cfg.Active[k] = *in.Active[k]
		case overrides.Active[k] != nil:
			cfg.Active[k] = *overrides.Active[k]
		case !slices.Contains(cfg.Selected[k], cfg.Active[k]):
			cfg.Active[k] = ""
			if len(cfg.Selected[k]) > 0 {
				cfg.Active[k] = cfg.Selected[k][0]
			}
		}
	}
	if in.DefaultLLMs != nil {
		ids := *in.DefaultLLMs
		seen := map[string]bool{}
		for _, id := range ids {
			s := cat.ByID(id)
			switch {
			case !slices.Contains(cfg.Selected["llm"], id):
				return warnings, &DocumentError{Message: fmt.Sprintf("defaultLlmModels: %s must also be present in selectedLlmModels", id)}
			case s != nil && (s.IsDecision() || s.IsEmbedding()):
				return warnings, &DocumentError{Message: fmt.Sprintf("defaultLlmModels: %s must name a chat model", id)}
			case seen[id]:
				return warnings, &DocumentError{Message: "defaultLlmModels: must not contain duplicates"}
			}
			seen[id] = true
		}
		cfg.DefaultLLMs = nil
		switch {
		case len(ids) == 1 && in.Active["llm"] != nil && ids[0] != cfg.Active["llm"]:
			return warnings, &DocumentError{Message: "defaultLlmModels: a single default must equal activeLlmModel"}
		case len(ids) == 1:
			cfg.Active["llm"] = ids[0]
		case len(ids) >= 2:
			// activeLlmModel stays as it is: TS launches it at start after
			// a rollback, without the fit fallback.
			cfg.DefaultLLMs = ids
		}
	}
	str := func(flag *string, toml *string, dst *string) {
		if flag != nil {
			*dst = *flag
		} else if toml != nil {
			*dst = *toml
		}
	}
	str(in.Host, overrides.Host, &cfg.Host)
	str(in.SttHost, overrides.SttHost, &cfg.SttHost)
	str(in.CtxMode, overrides.CtxMode, &cfg.CtxMode)
	str(in.OtelEndpoint, overrides.OtelEndpoint, &cfg.OtelEndpoint)
	str(in.OtelHeaders, overrides.OtelHeaders, &cfg.OtelHeaders)
	for _, f := range []struct {
		flag   string
		given  *string
		toml   *int
		dst    *int
		lo, hi int
	}{{"port", in.Port, overrides.Port, &cfg.Port, 1, 65535}, {"stt-port", in.SttPort, overrides.SttPort, &cfg.SttPort, 1, 65535},
		{"ctx-size", in.CtxSize, overrides.CtxSize, &cfg.CtxSize, 1, 2_147_483_647}, {"otel-sample-ratio", in.OtelSampleRatio, overrides.OtelSampleRatio, &cfg.OtelSampleRatio, 0, 100}} {
		v, err := intInput(f.flag, f.given, f.lo, f.hi)
		if err != nil {
			return warnings, err
		}
		if v != nil {
			*f.dst = *v
		} else if f.toml != nil {
			*f.dst = *f.toml
		}
	}
	if in.CtxMode != nil && cfg.CtxMode != "auto" && cfg.CtxMode != "explicit" {
		return warnings, &DocumentError{Message: `ctxMode: Invalid option: expected one of "auto"|"explicit"`}
	}
	if in.OtelEndpoint != nil && cfg.OtelEndpoint != "" {
		if msg := obs.ValidateEndpoint(cfg.OtelEndpoint); msg != "" {
			return warnings, &DocumentError{Message: "otelEndpoint: " + msg}
		}
	}
	if in.OtelHeaders != nil {
		if _, err := obs.ParseHeaders(cfg.OtelHeaders); err != nil {
			return warnings, &DocumentError{Message: "otelHeaders: " + err.Error()}
		}
	}
	warnings = append(warnings, secretOptionWarnings(in)...)
	parallel := ""
	if in.Parallel != nil {
		parallel = *in.Parallel
	}
	if in.Parallel == nil && overrides.Parallel != nil {
		parallel = *overrides.Parallel
	}
	if parallel != "" {
		p := strings.ToLower(strings.TrimSpace(parallel))
		if p != "auto" && !slices.Contains([]string{"1", "2", "3", "4"}, p) {
			return warnings, &DocumentError{Message: fmt.Sprintf("Invalid parallel value: %s. Use 'auto' or an integer from 1 to 4.", parallel)}
		}
		cfg.Parallel = p
	}
	if overrides.SystemReserve != nil {
		cfg.SystemReserve = *overrides.SystemReserve
	}
	if overrides.AcceleratorReserve != nil {
		cfg.AcceleratorReserve = *overrides.AcceleratorReserve
	}
	// --hf-token always replaces ("" clears); a TOML token only fills a missing one.
	switch {
	case in.HFToken != nil:
		cfg.HFToken = *in.HFToken
	case overrides.HFToken != nil:
		if cfg.HFToken == "" || cfg.HFToken == *overrides.HFToken {
			cfg.HFToken = *overrides.HFToken
		} else {
			warnings = append(warnings, "Warning: the hfToken in the TOML file was not applied because a different Hugging Face token is already saved, for example from the admin UI. Replace it there, or pass --hf-token.")
		}
	case cfg.HFToken == "" && !in.ExistingRoot:
		cfg.HFToken = in.HFTokenEnv
	}
	if issues := ValidateModels(cfg, cat, false); len(issues) > 0 {
		return warnings, &DocumentError{Message: Summary(issues)}
	}
	if p, err := strconv.Atoi(cfg.Parallel); err == nil && p > 1 && in.MemoryGB < 16 {
		warnings = append(warnings, fmt.Sprintf("Warning: Setting parallel slots to %d on a system with only %d GB VRAM may cause Out-Of-Memory (OOM) crashes.", p, in.MemoryGB))
	}
	return warnings, nil
}

// intInput is an integer option as given (nil when not), checked against its range, as cli's Int
// and intFlag read one from the command line.
func intInput(name string, given *string, lo, hi int) (*int, error) {
	if given == nil {
		return nil, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(*given))
	if err != nil {
		return nil, &DocumentError{Message: fmt.Sprintf("--%s must be an integer", name)}
	}
	if n < lo || n > hi {
		return nil, &DocumentError{Message: fmt.Sprintf("%s: must be from %d to %d", name, lo, hi)}
	}
	return &n, nil
}

// secretOptionWarnings say what a secret on the command line costs. Clearing one with an empty
// value costs nothing, and the value itself is never repeated.
func secretOptionWarnings(in Input) []string {
	var out []string
	for _, o := range []struct {
		name    string
		given   *string
		instead string
	}{{"otel-headers", in.OtelHeaders, "Set OTEL_EXPORTER_OTLP_HEADERS, or use --config"}, {"hf-token", in.HFToken, "Set HF_TOKEN, or save it in Settings"}} {
		if o.given != nil && *o.given != "" {
			out = append(out, fmt.Sprintf("Warning: --%s can be read by other users of this computer while the command runs, and stays in your shell history. %s.", o.name, o.instead))
		}
	}
	return out
}
