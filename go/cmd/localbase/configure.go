package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/conf"
	"github.com/timsexperiments/LocalBase/go/internal/lease"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// configure is flags-only: it behaves as the TS command does under
// --non-interactive and never prompts (DESIGN §2.8).
func configureCommand() *cli.Command {
	opt := func(name, value, help string) cli.Option { return cli.Option{Name: name, Value: value, Help: help} }
	return &cli.Command{Name: "configure", Summary: "Configure models, ports, settings, and API keys",
		Examples: []string{"local-base configure --defaults --parallel auto",
			"local-base configure --llm-models qwen3.5-4b-q4_k_m --active-llm qwen3.5-4b-q4_k_m"},
		Options: []cli.Option{
			{Name: "all", Bool: true, Help: "Accepted for compatibility; Go never prompts"},
			{Name: "defaults", Bool: true, Help: "Use saved or default settings"},
			opt("config", "<file>", "Load TOML configuration overrides"),
			opt("host", "<host>", "LLM host"), opt("port", "<port>", "LLM port"),
			opt("ctx-size", "<tokens>", "LLM context limit ceiling"),
			opt("ctx-mode", "<auto|explicit>", "auto clamps ctx-size to the memory-safe recommendation; explicit launches at ctx-size"),
			opt("parallel", "<auto|1-4>", "Parallel request slots"),
			opt("max-connections", "<connections>", "Maximum open connections, 0 for no cap; applies on restart (Go only)"),
			opt("max-connections-per-ip", "<connections>", "Maximum open connections from one address other than loopback, 0 for no cap; applies on restart (Go only)"),
			opt("import-deadline-hours", "<hours>", "How long one import may run before it stops and can resume (default 48); applies to the next import (Go only)"),
			opt("import-max-unknown-gib", "<GiB>", "Largest file an import may download when its source does not say the size (default 256). A file with a stated size is limited only by free disk; applies to the next import (Go only)"),
			{Name: "allow-private-imports", Bool: true, Help: "Let imports and catalog installs reach private addresses, such as a LAN mirror (--no-allow-private-imports: do not). Off by default (Go only)"},
			{Name: "allow-local-path-imports", Bool: true, Help: "Let the API and pages import files from this computer, with the size and checksum they report (--no-allow-local-path-imports: do not). Off by default; the CLI always can (Go only)"},
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
			opt("default-llm-models", "<id,...>", "Ordered default chat models: new conversations use the first that fits in memory now; one ID is the active LLM; empty clears (Go only)"),
			opt("hf-token", "<token>", "Hugging Face token for gated downloads"),
			{Name: "create-key", Bool: true, Help: "Create an initial API key (--no-create-key: do not)"},
		},
		Run: runConfigure}
}

func runConfigure(c *cli.Ctx) (any, error) {
	var overrides *conf.Overrides
	if p := c.String("config"); c.Has("config") {
		var err error
		if overrides, err = loadOverrides(p); err != nil {
			return nil, err
		}
	}
	if overrides != nil && overrides.Root != nil && c.Global.Root == "" && os.Getenv("LOCALBASE_ROOT") == "" {
		c.Global.Root = *overrides.Root
	}
	// TS assertCatalogModels runs in the command's validate step, before
	// the root's catalog sources load: only built-in ids pass it.
	if builtIn, err := catalog.Load(""); err == nil {
		for _, k := range []string{"llm", "stt", "tts", "image"} {
			if !c.Has(k + "-models") {
				continue
			}
			if invalid := conf.NotBuiltIn(builtIn, k, cli.SplitList(c.String(k+"-models"))); len(invalid) > 0 {
				return nil, cli.Inputf("Invalid %s model ids: %s", k, strings.Join(invalid, ", "))
			}
		}
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
	warnings, err := conf.MergeInput(cfg, configureInputOf(c, hasConfig, e.memoryGB()), overrides, e.Catalog)
	for _, w := range warnings {
		c.Error("%s", w)
	}
	if err != nil {
		return nil, inputError(err)
	}
	// Every setting option is checked before anything is written, then the configuration and the
	// settings go in together, in this order and in one transaction: a refused command writes
	// nothing, and neither does one whose write fails partway.
	type setting struct {
		key, value string
		toggle     bool // an import opt-in: a change to it is audited
	}
	var settings []setting
	for _, o := range []struct {
		flag, key string
		low       int
	}{
		{"max-connections", store.MaxConnectionsKey, 0}, {"max-connections-per-ip", store.MaxConnectionsPerIPKey, 0},
		{"import-deadline-hours", store.ImportDeadlineKey, 1}, {"import-max-unknown-gib", store.ImportMaxUnknownKey, 1},
	} {
		if c.Has(o.flag) {
			n, err := intOpt(c, o.flag, o.low, 10_000_000)
			if err != nil {
				return nil, err
			}
			settings = append(settings, setting{key: o.key, value: strconv.Itoa(n)})
		}
	}
	for _, o := range []struct{ flag, key string }{
		{"allow-private-imports", store.PrivateImportsKey}, {"allow-local-path-imports", store.LocalPathImportsKey},
	} {
		if v, ok := c.OptBool(o.flag); ok {
			settings = append(settings, setting{key: o.key, value: map[bool]string{true: "on", false: "off"}[v], toggle: true})
		}
	}
	var changed []setting // the toggles whose value this command changes; unset counts as off
	if err := lease.WithOperation(e.Root.Dir, "configure", func() error {
		for _, s := range settings {
			if !s.toggle {
				continue
			}
			was, _, err := e.Store.Setting(context.Background(), s.key)
			if err != nil {
				return err
			}
			if conf.ToggleChanged(was, s.value) {
				changed = append(changed, s)
			}
		}
		values := make([]store.SettingValue, len(settings))
		for i, s := range settings {
			values[i] = store.SettingValue{Key: s.key, Value: s.value}
		}
		return e.saveAs(cfg, true, values...)
	}); err != nil {
		var inv *conf.InvalidError
		if errors.As(err, &inv) {
			return nil, cli.Inputf("%s", conf.Summary(inv.Issues))
		}
		return nil, err
	}
	e.audit.Change("config", "configure", "configuration", "")
	for _, s := range changed {
		e.audit.Change("config", "set-setting", "setting", s.key, "setting", s.key, "value", s.value)
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
		e.audit.KeyChange("create", k, "purpose", "initial")
		c.Info("\nCreated initial API key:")
		c.Info("id=%s name=%s prefix=%s", k.ID, k.Name, k.Prefix)
		if !c.Global.JSON {
			c.Info("secret=%s", raw)
			c.Info("Store this secret now. It is not shown again.")
		}
		created := auth.PublicKey(k)
		created["secret"] = raw
		data["createdKey"] = created
	}
	return e.audit.WithResult(data), nil
}

// configureInputOf is what configure was given on the command line.
func configureInputOf(c *cli.Ctx, existingRoot bool, memoryGB int) conf.Input {
	given := func(name string) *string {
		if !c.Has(name) {
			return nil
		}
		v := c.String(name)
		return &v
	}
	in := conf.Input{Models: map[string][]string{}, Active: map[string]*string{},
		Host: given("host"), SttHost: given("stt-host"), CtxMode: given("ctx-mode"), OtelEndpoint: given("otel-endpoint"),
		OtelHeaders: given("otel-headers"), Parallel: given("parallel"), HFToken: given("hf-token"),
		Port: given("port"), SttPort: given("stt-port"), CtxSize: given("ctx-size"), OtelSampleRatio: given("otel-sample-ratio"),
		ExistingRoot: existingRoot, HFTokenEnv: os.Getenv("HF_TOKEN"), MemoryGB: memoryGB}
	for _, k := range kinds {
		if c.Has(k + "-models") {
			ids := cli.SplitList(c.String(k + "-models"))
			if ids == nil {
				ids = []string{}
			}
			in.Models[k] = ids
		}
		in.Active[k] = given("active-" + k)
	}
	if c.Has("default-llm-models") {
		ids := cli.SplitList(c.String("default-llm-models"))
		in.DefaultLLMs = &ids
	}
	return in
}

var kindLabel = map[string]string{"llm": "LLM", "stt": "STT", "tts": "TTS", "image": "Image", "video": "Video"}

func rootDBPath(c *cli.Ctx) string {
	dir := root.Resolve(c.Global.Root)
	if canon, err := root.Canonical(dir); err == nil {
		dir = canon
	}
	return filepath.Join(dir, "local-base.db")
}

// loadOverrides reads a configure TOML file (TS loadTomlOverrides). A file that isn't valid is bad
// input (exit 2), with conf.ParseOverrides's message.
func loadOverrides(path string) (*conf.Overrides, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("Config file not found: %s", path)
	}
	if err != nil {
		return nil, err
	}
	o, err := conf.ParseOverrides(string(b))
	if err != nil {
		return nil, inputError(err)
	}
	return o, nil
}
