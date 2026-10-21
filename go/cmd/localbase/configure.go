package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/conf"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/toml"
)

// configure is flags-only: it behaves as the TS command does under
// --non-interactive and never prompts (DESIGN §2.8).
func configureCommand() *cli.Command {
	opt := func(name, value, help string) cli.Option { return cli.Option{Name: name, Value: value, Help: help} }
	return &cli.Command{Name: "configure", Summary: "Configure models, ports, settings, and API keys",
		Examples: []string{"local-base configure --defaults --parallel auto",
			"local-base configure --llm-models qwen2.5-coder-7b-instruct-q4_k_m --active-llm qwen2.5-coder-7b-instruct-q4_k_m"},
		Options: []cli.Option{
			{Name: "all", Bool: true, Help: "Accepted for compatibility; Go never prompts"},
			{Name: "defaults", Bool: true, Help: "Use saved or default settings"},
			opt("config", "<file>", "Load TOML configuration overrides"),
			opt("host", "<host>", "LLM host"), opt("port", "<port>", "LLM port"),
			opt("ctx-size", "<tokens>", "LLM context limit ceiling"),
			opt("ctx-mode", "<auto|explicit>", "auto clamps ctx-size to the memory-safe recommendation; explicit launches at ctx-size"),
			opt("parallel", "<auto|1-4>", "Parallel request slots"),
			opt("otel-endpoint", "<url>", "OTLP/HTTP base endpoint; empty disables export"),
			opt("otel-headers", "<key=value,...>", "OTLP exporter headers"),
			opt("otel-sample-ratio", "<0-100>", "Percentage of root traces to sample"),
			opt("stt-host", "<host>", "STT host"), opt("stt-port", "<port>", "STT port"),
			opt("llm-models", "<id,...>", "Selected LLM model IDs"),
			opt("stt-models", "<id,...>", "Selected STT model IDs; use an empty value to disable"),
			opt("tts-models", "<id,...>", "Selected TTS model IDs; use an empty value to disable"),
			opt("image-models", "<id,...>", "Selected image model IDs; use an empty value to disable"),
			opt("video-models", "<id,...>", "Selected video model IDs; use an empty value to disable"),
			opt("active-llm", "<id>", "Active LLM model ID"), opt("active-stt", "<id>", "Active STT model ID"),
			opt("active-tts", "<id>", "Active TTS model ID"), opt("active-image", "<id>", "Active image model ID"),
			opt("active-video", "<id>", "Active video model ID"),
			opt("hf-token", "<token>", "Hugging Face token for gated downloads"),
			{Name: "create-key", Bool: true, Help: "Create an initial API key (--no-create-key: do not)"},
		},
		Run: runConfigure}
}

func intFlag(c *cli.Ctx, name string, lo, hi int) (*int, error) {
	n, ok, err := c.Int(name)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	if n < lo || n > hi {
		return nil, cli.Inputf("%s: must be from %d to %d", name, lo, hi)
	}
	return &n, nil
}

func runConfigure(c *cli.Ctx) (any, error) {
	var overrides *Overrides
	if p := c.String("config"); c.Has("config") {
		var err error
		if overrides, err = loadOverrides(p); err != nil {
			return nil, err
		}
	}
	if overrides != nil && overrides.Root != nil && c.Global.Root == "" && os.Getenv("LOCALBASE_ROOT") == "" {
		c.Global.Root = *overrides.Root
	}
	hasConfig := false
	if _, err := os.Stat(rootDBPath(c)); err == nil {
		hasConfig = true
	}
	e, err := openEnv(c)
	if err != nil {
		return nil, err
	}
	defer e.Close()
	cfg := e.Config.Clone()
	if overrides == nil {
		overrides = &Overrides{}
	}

	lists := map[string][]string{}
	for _, k := range kinds {
		name := k + "-models"
		if !c.Has(name) {
			continue
		}
		ids := cli.SplitList(c.String(name))
		if ids == nil {
			ids = []string{}
		}
		if k == "llm" && len(ids) == 0 {
			return nil, cli.Inputf("llmModels: Too small: expected array to have >=1 items")
		}
		probe := cfg.Clone()
		probe.Selected = map[string][]string{k: ids}
		probe.Active = map[string]string{}
		for _, is := range conf.ValidateModels(probe, e.Catalog, k != "video") {
			if strings.HasPrefix(is.Path, "selected"+fieldTitle(k)+"Models") {
				return nil, cli.Inputf("%s", is.String())
			}
		}
		lists[k] = ids
	}
	for _, k := range kinds {
		if ids, ok := lists[k]; ok {
			cfg.Selected[k] = ids
		} else if ids := overrides.Selected[k]; ids != nil {
			cfg.Selected[k] = ids
		}
		flag := "active-" + k
		switch {
		case c.Has(flag):
			if c.String(flag) == "" {
				return nil, cli.Inputf("active%s: Too small: expected string to have >=1 characters", fieldTitle(k))
			}
			cfg.Active[k] = c.String(flag)
		case overrides.Active[k] != nil:
			cfg.Active[k] = *overrides.Active[k]
		case !slices.Contains(cfg.Selected[k], cfg.Active[k]):
			cfg.Active[k] = ""
			if len(cfg.Selected[k]) > 0 {
				cfg.Active[k] = cfg.Selected[k][0]
			}
		}
	}
	str := func(flag string, toml *string, dst *string) {
		if c.Has(flag) {
			*dst = c.String(flag)
		} else if toml != nil {
			*dst = *toml
		}
	}
	str("host", overrides.Host, &cfg.Host)
	str("stt-host", overrides.SttHost, &cfg.SttHost)
	str("ctx-mode", overrides.CtxMode, &cfg.CtxMode)
	str("otel-endpoint", overrides.OtelEndpoint, &cfg.OtelEndpoint)
	str("otel-headers", overrides.OtelHeaders, &cfg.OtelHeaders)
	for _, f := range []struct {
		flag   string
		toml   *int
		dst    *int
		lo, hi int
	}{{"port", overrides.Port, &cfg.Port, 1, 65535}, {"stt-port", overrides.SttPort, &cfg.SttPort, 1, 65535},
		{"ctx-size", overrides.CtxSize, &cfg.CtxSize, 1, 2_147_483_647}, {"otel-sample-ratio", overrides.OtelSampleRatio, &cfg.OtelSampleRatio, 0, 100}} {
		v, err := intFlag(c, f.flag, f.lo, f.hi)
		if err != nil {
			return nil, err
		}
		if v != nil {
			*f.dst = *v
		} else if f.toml != nil {
			*f.dst = *f.toml
		}
	}
	if c.Has("ctx-mode") && cfg.CtxMode != "auto" && cfg.CtxMode != "explicit" {
		return nil, cli.Inputf(`ctxMode: Invalid option: expected one of "auto"|"explicit"`)
	}
	if c.Has("otel-endpoint") && cfg.OtelEndpoint != "" {
		if msg := obs.ValidateEndpoint(cfg.OtelEndpoint); msg != "" {
			return nil, cli.Inputf("otelEndpoint: %s", msg)
		}
	}
	if c.Has("otel-headers") {
		if _, err := obs.ParseHeaders(cfg.OtelHeaders); err != nil {
			return nil, cli.Inputf("otelHeaders: %s", err.Error())
		}
	}
	parallel := c.String("parallel")
	if !c.Has("parallel") && overrides.Parallel != nil {
		parallel = *overrides.Parallel
	}
	if parallel != "" {
		p := strings.ToLower(strings.TrimSpace(parallel))
		if p != "auto" && !slices.Contains([]string{"1", "2", "3", "4"}, p) {
			return nil, cli.Inputf("Invalid parallel value: %s. Use 'auto' or an integer from 1 to 4.", parallel)
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
	case c.Has("hf-token"):
		cfg.HFToken = c.String("hf-token")
	case overrides.HFToken != nil:
		if cfg.HFToken == "" || cfg.HFToken == *overrides.HFToken {
			cfg.HFToken = *overrides.HFToken
		} else {
			fmt.Fprintln(c.Stderr, "Warning: the hfToken in the TOML file was not applied because a different Hugging Face token is already saved, for example from the admin UI. Replace it there, or pass --hf-token.")
		}
	case cfg.HFToken == "" && !hasConfig:
		cfg.HFToken = os.Getenv("HF_TOKEN")
	}
	if issues := conf.ValidateModels(cfg, e.Catalog, false); len(issues) > 0 {
		return nil, cli.Inputf("%s", conf.Summary(issues))
	}
	if p, err := strconv.Atoi(cfg.Parallel); err == nil && p > 1 && e.memoryGB() < 16 {
		fmt.Fprintf(c.Stderr, "Warning: Setting parallel slots to %d on a system with only %d GB VRAM may cause Out-Of-Memory (OOM) crashes.\n", p, e.memoryGB())
	}
	if err := root.WithOperation(e.Root.Dir, "configure", func() error { return e.saveAs(cfg, true) }); err != nil {
		var inv *conf.InvalidError
		if errors.As(err, &inv) {
			return nil, cli.Inputf("%s", conf.Summary(inv.Issues))
		}
		return nil, err
	}
	c.Info("Saved configuration to %s/local-base.db", cfg.Root)
	for _, k := range kinds {
		if len(cfg.Selected[k]) > 0 {
			c.Info("Selected %s models: %s", kindLabel[k], strings.Join(cfg.Selected[k], ", "))
		}
	}
	eff, err := obs.ResolveOtel(cfg.OtelEndpoint, cfg.OtelHeaders, cfg.OtelSampleRatio, os.Getenv)
	if err != nil {
		return nil, err
	}
	data := map[string]any{"configuration": conf.Public(cfg, eff)}
	keys, err := e.Store.ListAPIKeys(context.Background())
	if err != nil {
		return nil, err
	}
	active := false
	for _, k := range keys {
		active = active || k.RevokedAt == nil
	}
	create := !c.Global.JSON
	if v, ok := c.OptBool("create-key"); ok {
		create = v
	}
	if !active && create {
		k, raw, err := e.Store.CreateAPIKey(context.Background(), "default", auth.DefaultKeyScopes)
		if err != nil {
			return nil, err
		}
		c.Info("\nCreated initial API key:")
		c.Info("id=%s name=%s prefix=%s", k.ID, k.Name, k.Prefix)
		if !c.Global.JSON {
			c.Info("secret=%s", raw)
			c.Info("Store this secret now. It is not shown again.")
		}
		created := publicKey(k)
		created["secret"] = raw
		data["createdKey"] = created
	}
	return data, nil
}

var kindLabel = map[string]string{"llm": "LLM", "stt": "STT", "tts": "TTS", "image": "Image", "video": "Video"}

func fieldTitle(k string) string {
	return map[string]string{"llm": "Llm", "stt": "Stt", "tts": "Tts", "image": "Image", "video": "Video"}[k]
}

func rootDBPath(c *cli.Ctx) string {
	dir := root.Resolve(c.Global.Root)
	if canon, err := root.Canonical(dir); err == nil {
		dir = canon
	}
	return filepath.Join(dir, "local-base.db")
}

// Overrides are the configure settings a TOML file may carry.
type Overrides struct {
	Root, Host, SttHost, CtxMode, OtelEndpoint, OtelHeaders, HFToken, Parallel *string
	Port, SttPort, CtxSize, OtelSampleRatio                                    *int
	Selected                                                                   map[string][]string
	Active                                                                     map[string]*string
	SystemReserve, AcceleratorReserve                                          *store.MemoryReserve
}

// loadOverrides reads a configure TOML file (TS loadTomlOverrides): known
// keys only, checked by type and range.
func loadOverrides(path string) (*Overrides, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("Config file not found: %s", path)
	}
	if err != nil {
		return nil, err
	}
	doc, err := toml.Parse(string(b))
	if err != nil {
		return nil, cli.Inputf("Invalid TOML configuration: %v", err)
	}
	o := &Overrides{Selected: map[string][]string{}, Active: map[string]*string{}}
	var issues []string
	bad := func(key, msg string) { issues = append(issues, key+": "+msg) }
	str := func(key string, v any) *string {
		s, ok := v.(string)
		if !ok {
			bad(key, "Invalid input: expected string")
			return nil
		}
		return &s
	}
	num := func(key string, v any, lo, hi int64) *int {
		n, ok := v.(int64)
		if !ok || n < lo || n > hi {
			bad(key, fmt.Sprintf("must be an integer from %d to %d", lo, hi))
			return nil
		}
		i := int(n)
		return &i
	}
	for _, k := range toml.SortedKeys(doc) {
		v := doc[k]
		switch k {
		case "root":
			o.Root = str(k, v)
		case "host":
			o.Host = str(k, v)
		case "sttHost":
			o.SttHost = str(k, v)
		case "hfToken":
			o.HFToken = str(k, v)
		case "otelEndpoint":
			o.OtelEndpoint = str(k, v)
			if o.OtelEndpoint != nil && *o.OtelEndpoint != "" {
				if msg := obs.ValidateEndpoint(*o.OtelEndpoint); msg != "" {
					bad(k, msg)
				}
			}
		case "otelHeaders":
			if o.OtelHeaders = str(k, v); o.OtelHeaders != nil {
				if _, err := obs.ParseHeaders(*o.OtelHeaders); err != nil {
					bad(k, err.Error())
				}
			}
		case "ctxMode":
			if o.CtxMode = str(k, v); o.CtxMode != nil && *o.CtxMode != "auto" && *o.CtxMode != "explicit" {
				bad(k, `Invalid option: expected one of "auto"|"explicit"`)
			}
		case "parallel":
			switch p := v.(type) {
			case string:
				if p != "auto" {
					bad(k, "must be \"auto\" or 1-4")
				}
				o.Parallel = &p
			case int64:
				if p < 1 || p > 4 {
					bad(k, "must be \"auto\" or 1-4")
				}
				s := strconv.FormatInt(p, 10)
				o.Parallel = &s
			default:
				bad(k, "must be \"auto\" or 1-4")
			}
		case "port":
			o.Port = num(k, v, 1, 65535)
		case "sttPort":
			o.SttPort = num(k, v, 1, 65535)
		case "ctxSize":
			o.CtxSize = num(k, v, 1, 2_147_483_647)
		case "otelSampleRatio":
			o.OtelSampleRatio = num(k, v, 0, 100)
		case "memory":
			m, ok := v.(map[string]any)
			if !ok {
				bad(k, "Invalid input: expected object")
				continue
			}
			for _, rk := range toml.SortedKeys(m) {
				r, ok := m[rk].(map[string]any)
				if !ok || (rk != "systemReserve" && rk != "acceleratorReserve") {
					bad("memory."+rk, "Unrecognized key")
					continue
				}
				res := store.MemoryReserve{Percent: 15, MinimumGB: 8}
				if rk == "acceleratorReserve" {
					res = store.MemoryReserve{Percent: 10, MinimumGB: 2}
				}
				for fk, fv := range r {
					f, isF := fv.(float64)
					if n, isN := fv.(int64); isN {
						f, isF = float64(n), true
					}
					switch {
					case !isF:
						bad("memory."+rk+"."+fk, "Invalid input: expected number")
					case fk == "percent" && f >= 0 && f <= 100:
						res.Percent = f
					case fk == "minimumGb" && f >= 0:
						res.MinimumGB = f
					default:
						bad("memory."+rk+"."+fk, "Invalid value")
					}
				}
				if rk == "systemReserve" {
					o.SystemReserve = &res
				} else {
					o.AcceleratorReserve = &res
				}
			}
		default:
			kind := ""
			for _, kk := range kinds {
				if k == "selected"+fieldTitle(kk)+"Models" || k == "active"+fieldTitle(kk)+"Model" {
					kind = kk
				}
			}
			switch {
			case kind != "" && strings.HasPrefix(k, "selected"):
				list, ok := v.([]any)
				if !ok {
					bad(k, "Invalid input: expected array")
					continue
				}
				ids := []string{}
				for _, e := range list {
					s, ok := e.(string)
					if !ok {
						bad(k, "Invalid input: expected string")
					}
					ids = append(ids, s)
				}
				o.Selected[kind] = ids
			case kind != "":
				o.Active[kind] = str(k, v)
			default:
				bad(k, "Unrecognized key")
			}
		}
	}
	if len(issues) > 0 {
		return nil, cli.Inputf("%s", strings.Join(issues, "; "))
	}
	return o, nil
}
