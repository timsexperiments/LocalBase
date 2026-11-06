package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// MemoryReserve is a pool reserve: the larger of a percentage of capacity and a floor.
type MemoryReserve struct {
	Percent   float64 `json:"percent"`
	MinimumGB float64 `json:"minimumGb"`
}

// Config is the single `config` row (id "default").
type Config struct {
	Root                 string
	GatewayHost          string
	GatewayPort          int
	Host                 string // LLM backend host
	Port                 int    // LLM backend port
	CtxSize              int
	CtxMode              string // "auto" | "explicit"
	SttHost              string
	SttPort              int
	Selected             map[string][]string // kind -> selected ids
	Active               map[string]string   // kind -> active id
	AllowExperimental    bool
	HFToken              string
	InstallMissingModels bool
	Parallel             string // "auto" | "1".."4"
	OtelEndpoint         string
	OtelHeaders          string
	OtelSampleRatio      int
	SystemReserve        MemoryReserve
	AcceleratorReserve   MemoryReserve
	// DefaultLLMs is the ordered default chat model list (Go only, stored in
	// go_settings, DESIGN §18): two or more ids; nil means the active LLM
	// alone, as in TS.
	DefaultLLMs []string
}

// DefaultLLMsKey is the go_settings key of Config.DefaultLLMs.
const DefaultLLMsKey = "models.default-llm-models"

// DefaultLLMList is the ordered default chat model list: DefaultLLMs, or
// the active LLM alone.
func (c *Config) DefaultLLMList() []string {
	if len(c.DefaultLLMs) >= 2 {
		return c.DefaultLLMs
	}
	return []string{c.Active["llm"]}
}

// Clone copies the config, including its maps.
func (c *Config) Clone() *Config {
	n := *c
	n.Selected, n.Active = map[string][]string{}, map[string]string{}
	n.DefaultLLMs = slices.Clone(c.DefaultLLMs)
	for k, v := range c.Selected {
		n.Selected[k] = append([]string{}, v...)
	}
	for k, v := range c.Active {
		n.Active[k] = v
	}
	return &n
}

// Kinds in served order.
var Kinds = []string{"llm", "stt", "tts", "image", "video"}

var ErrNotConfigured = errors.New("LocalBase is not configured; run `local-base configure`")

const configColumns = `root, gateway_host, gateway_port, host, port, ctx_size, ctx_mode, stt_host, stt_port,
	selected_llm_models, selected_stt_models, selected_tts_models, selected_image_models, selected_video_models,
	allow_experimental, active_llm_model, active_stt_model, active_tts_model, active_image_model, active_video_model,
	hf_token, install_missing_models, parallel, otel_endpoint, otel_headers, otel_sample_ratio,
	memory_system_reserve_percent, memory_system_reserve_minimum_gb,
	memory_accelerator_reserve_percent, memory_accelerator_reserve_minimum_gb`

// LoadConfig reads the config row. Row-level checks only; catalog checks
// live in package conf.
func (s *Store) LoadConfig(ctx context.Context) (*Config, error) {
	row := s.DB.QueryRowContext(ctx, `SELECT `+configColumns+` FROM config WHERE id = 'default'`)
	c := &Config{Selected: map[string][]string{}, Active: map[string]string{}}
	var sel, act [5]string
	err := row.Scan(&c.Root, &c.GatewayHost, &c.GatewayPort, &c.Host, &c.Port, &c.CtxSize, &c.CtxMode,
		&c.SttHost, &c.SttPort, &sel[0], &sel[1], &sel[2], &sel[3], &sel[4],
		&c.AllowExperimental, &act[0], &act[1], &act[2], &act[3], &act[4],
		&c.HFToken, &c.InstallMissingModels, &c.Parallel, &c.OtelEndpoint, &c.OtelHeaders, &c.OtelSampleRatio,
		&c.SystemReserve.Percent, &c.SystemReserve.MinimumGB,
		&c.AcceleratorReserve.Percent, &c.AcceleratorReserve.MinimumGB)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotConfigured
	}
	if err != nil {
		return nil, err
	}
	for i, kind := range Kinds {
		var ids []string
		if err := json.Unmarshal([]byte(sel[i]), &ids); err != nil {
			return nil, fmt.Errorf("config selected_%s_models: %w", kind, err)
		}
		if ids == nil {
			ids = []string{}
		}
		c.Selected[kind] = ids
		c.Active[kind] = act[i]
	}
	if c.Active["llm"] == "" {
		return nil, errors.New("config active_llm_model is empty")
	}
	// A database TS created and Go has only read has no go_settings table.
	if v, ok, err := s.Setting(ctx, DefaultLLMsKey); err == nil && ok {
		var ids []string
		if json.Unmarshal([]byte(v), &ids) == nil && len(ids) >= 2 {
			c.DefaultLLMs = ids
		}
	}
	return c, nil
}

// HasConfig reports whether the config row exists.
func (s *Store) HasConfig(ctx context.Context) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM config WHERE id = 'default'`).Scan(&n)
	return n > 0, err
}

// SaveConfig writes the config row (insert or replace every column).
func (s *Store) SaveConfig(ctx context.Context, c *Config) error { return saveConfig(ctx, s.DB, c) }

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func saveConfig(ctx context.Context, db execer, c *Config) error {
	var sel [5]string
	for i, kind := range Kinds {
		ids := c.Selected[kind]
		if ids == nil {
			ids = []string{}
		}
		b, _ := json.Marshal(ids)
		sel[i] = string(b)
	}
	b2i := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	_, err := db.ExecContext(ctx, `INSERT INTO config (id, `+configColumns+`) VALUES ('default',
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET root = excluded.root, gateway_host = excluded.gateway_host,
		gateway_port = excluded.gateway_port, host = excluded.host, port = excluded.port, ctx_size = excluded.ctx_size,
		ctx_mode = excluded.ctx_mode, stt_host = excluded.stt_host, stt_port = excluded.stt_port,
		selected_llm_models = excluded.selected_llm_models, selected_stt_models = excluded.selected_stt_models,
		selected_tts_models = excluded.selected_tts_models, selected_image_models = excluded.selected_image_models,
		selected_video_models = excluded.selected_video_models, allow_experimental = excluded.allow_experimental,
		active_llm_model = excluded.active_llm_model, active_stt_model = excluded.active_stt_model,
		active_tts_model = excluded.active_tts_model, active_image_model = excluded.active_image_model,
		active_video_model = excluded.active_video_model, hf_token = excluded.hf_token,
		install_missing_models = excluded.install_missing_models, parallel = excluded.parallel,
		otel_endpoint = excluded.otel_endpoint, otel_headers = excluded.otel_headers,
		otel_sample_ratio = excluded.otel_sample_ratio,
		memory_system_reserve_percent = excluded.memory_system_reserve_percent,
		memory_system_reserve_minimum_gb = excluded.memory_system_reserve_minimum_gb,
		memory_accelerator_reserve_percent = excluded.memory_accelerator_reserve_percent,
		memory_accelerator_reserve_minimum_gb = excluded.memory_accelerator_reserve_minimum_gb`,
		c.Root, c.GatewayHost, c.GatewayPort, c.Host, c.Port, c.CtxSize, c.CtxMode, c.SttHost, c.SttPort,
		sel[0], sel[1], sel[2], sel[3], sel[4], b2i(c.AllowExperimental),
		c.Active["llm"], c.Active["stt"], c.Active["tts"], c.Active["image"], c.Active["video"],
		c.HFToken, b2i(c.InstallMissingModels), c.Parallel, c.OtelEndpoint, c.OtelHeaders, c.OtelSampleRatio,
		c.SystemReserve.Percent, c.SystemReserve.MinimumGB, c.AcceleratorReserve.Percent, c.AcceleratorReserve.MinimumGB)
	if err != nil {
		return err
	}
	// A default that is no longer selected leaves the list; one left is
	// the active LLM alone.
	var defaults []string
	for _, id := range c.DefaultLLMs {
		if slices.Contains(c.Selected["llm"], id) {
			defaults = append(defaults, id)
		}
	}
	if len(defaults) >= 2 {
		b, _ := json.Marshal(defaults)
		_, err = db.ExecContext(ctx, `INSERT INTO go_settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, DefaultLLMsKey, string(b), nowISO())
	} else {
		_, err = db.ExecContext(ctx, `DELETE FROM go_settings WHERE key = ?`, DefaultLLMsKey)
	}
	return err
}
