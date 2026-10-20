package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// MemoryReserve is a pool reserve: the larger of a percentage of capacity and a floor.
type MemoryReserve struct {
	Percent   float64
	MinimumGB float64
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
	InstallMissingModels bool
	Parallel             string // "auto" | "1".."4"
	OtelEndpoint         string
	OtelSampleRatio      int
	SystemReserve        MemoryReserve
	AcceleratorReserve   MemoryReserve
}

// Kinds in served order.
var Kinds = []string{"llm", "stt", "tts", "image", "video"}

var ErrNotConfigured = errors.New("LocalBase is not configured; run `local-base configure`")

func (s *Store) LoadConfig(ctx context.Context) (*Config, error) {
	row := s.DB.QueryRowContext(ctx, `SELECT root, gateway_host, gateway_port, host, port, ctx_size, ctx_mode,
		stt_host, stt_port,
		selected_llm_models, selected_stt_models, selected_tts_models, selected_image_models, selected_video_models,
		active_llm_model, active_stt_model, active_tts_model, active_image_model, active_video_model,
		allow_experimental, install_missing_models, parallel, otel_endpoint, otel_sample_ratio,
		memory_system_reserve_percent, memory_system_reserve_minimum_gb,
		memory_accelerator_reserve_percent, memory_accelerator_reserve_minimum_gb
		FROM config WHERE id = 'default'`)
	c := &Config{Selected: map[string][]string{}, Active: map[string]string{}}
	var sel [5]string
	var act [5]string
	err := row.Scan(&c.Root, &c.GatewayHost, &c.GatewayPort, &c.Host, &c.Port, &c.CtxSize, &c.CtxMode,
		&c.SttHost, &c.SttPort,
		&sel[0], &sel[1], &sel[2], &sel[3], &sel[4],
		&act[0], &act[1], &act[2], &act[3], &act[4],
		&c.AllowExperimental, &c.InstallMissingModels, &c.Parallel, &c.OtelEndpoint, &c.OtelSampleRatio,
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
		c.Selected[kind] = ids
		c.Active[kind] = act[i]
	}
	if c.Active["llm"] == "" {
		return nil, errors.New("config active_llm_model is empty")
	}
	return c, nil
}
