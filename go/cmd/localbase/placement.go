package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/placement"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// `models placement`: the CPU/GPU placement of one model on this device
// (DESIGN §20). It shows the stored choice, the split a running gateway
// loaded, and what the next load would do; with options it changes the
// stored choice, which applies on the next load.
func modelsPlacementCommand() *cli.Command {
	opt := func(name, value, help string) cli.Option { return cli.Option{Name: name, Value: value, Help: help} }
	return &cli.Command{Name: "placement", Summary: "Show or set where a model runs: GPU, CPU, or split", Usage: "<modelId>", MinArgs: 1, MaxArgs: 1,
		Examples: []string{"local-base models placement qwen3.5-9b-q4_k_m",
			"local-base models placement qwen3.5-9b-q4_k_m --mode custom --gpu-layers 20",
			"local-base models placement qwen3.5-9b-q4_k_m --reset"},
		Options: []cli.Option{
			opt("mode", "<auto|gpu|cpu|custom>", "auto: on the GPU when it fits, otherwise the smallest offload that fits; gpu: GPU only, never offload; cpu: CPU only; custom: the values below"),
			opt("cpu-moe-layers", "<N>", "custom: keep the expert weights of the first N layers in system memory (models with expert layers, discrete GPUs)"),
			opt("gpu-layers", "<N>", "custom: this many layers on the GPU, the rest in system memory"),
			opt("context-size", "<tokens>", "the shared context size for all models (saved as explicit)"),
			{Name: "reset", Bool: true, Help: "Back to auto"},
		},
		Run: runModelsPlacement}
}

func runModelsPlacement(c *cli.Ctx) (any, error) {
	e, err := openEnv(c)
	if err != nil {
		return nil, err
	}
	defer e.Close()
	id := c.Args[0]
	s := e.Catalog.ByID(id)
	if s == nil {
		return nil, cli.Inputf("Unknown model id: %s", id)
	}
	if s.Kind != "llm" {
		return nil, cli.Inputf("Placement applies to language models; %s is a %s model.", id, s.Kind)
	}
	lib, err := openLibrary(e)
	if err != nil {
		return nil, err
	}
	mem := memory.NewController(e.Host, memory.Reserve{Percent: e.Config.SystemReserve.Percent, MinimumGB: e.Config.SystemReserve.MinimumGB}, false)
	src := &runtime.HostPlacement{Store: e.Store, DeviceID: lib.DeviceID, Memory: mem}
	llm := runtime.NewLLM(runtime.LLMDeps{Root: e.Root.Dir, ModelsDir: e.Root.ModelsDir("llm"), RuntimesDir: e.Root.RuntimesDir(),
		Store: e.Store, Catalog: e.Catalog, Memory: mem, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		MemoryGB: float64(e.memoryGB()), Gate: lib.Gate(), Placement: src}, e.Config)
	defer llm.Close()
	live := gatewayPlacement(e, id)
	changing := c.Has("mode") || c.Has("cpu-moe-layers") || c.Has("gpu-layers") || c.Has("context-size") || c.Bool("reset")
	if changing && live != nil && live.Loading {
		return nil, cli.Inputf("The model is loading. Change its placement once it has loaded; the change then applies on the next load.")
	}
	ctx := context.Background()
	if c.Bool("reset") {
		if _, err := e.Store.ResetPlacement(ctx, id, lib.DeviceID); err != nil {
			return nil, err
		}
	} else if changing {
		cur := src.Request(id)
		req := placement.Request{Mode: cur.Mode}
		if c.Has("mode") {
			req.Mode = c.String("mode")
		}
		intOpt := func(name string) (*int, error) {
			if !c.Has(name) {
				return nil, nil
			}
			n, err := strconv.Atoi(c.String(name))
			if err != nil {
				return nil, cli.Inputf("--%s must be a whole number", name)
			}
			return &n, nil
		}
		if req.CPUMoELayers, err = intOpt("cpu-moe-layers"); err != nil {
			return nil, err
		}
		if req.GPULayers, err = intOpt("gpu-layers"); err != nil {
			return nil, err
		}
		if (req.CPUMoELayers != nil || req.GPULayers != nil) && req.Mode != placement.Custom {
			if c.Has("mode") {
				return nil, cli.Inputf("--cpu-moe-layers and --gpu-layers apply to --mode custom only.")
			}
			req.Mode = placement.Custom
		}
		if c.Has("context-size") {
			n, err := strconv.Atoi(c.String("context-size"))
			if err != nil || n < 256 {
				return nil, cli.Inputf("--context-size must be at least 256 tokens")
			}
			cfg := e.Config.Clone()
			cfg.CtxSize, cfg.CtxMode = n, "explicit"
			if err := e.save(cfg); err != nil {
				return nil, err
			}
			e.Config = cfg
			llm.SetConfig(cfg)
		}
		if c.Has("mode") || req.CPUMoELayers != nil || req.GPULayers != nil {
			if _, err := llm.PreviewPlacement(id, req); err != nil {
				var pe *placement.Error
				if errors.As(err, &pe) {
					return nil, cli.Inputf("%s", pe.Message)
				}
				return nil, err
			}
			if err := e.Store.SetPlacement(ctx, id, lib.DeviceID, store.Placement{Mode: req.Mode, GPULayers: req.GPULayers, CPUMoELayers: req.CPUMoELayers}); err != nil {
				return nil, err
			}
		}
	}
	st := placementStatus(e, llm, src, id, live)
	for _, l := range placementTextLines(st) {
		c.Info("%s", l)
	}
	return st, nil
}

// placementState is the JSON `models placement` returns.
type placementState struct {
	Model           string            `json:"model"`
	Requested       map[string]any    `json:"requested"`
	Loaded          bool              `json:"loaded"`
	Resolved        map[string]any    `json:"resolved,omitempty"`
	Next            map[string]any    `json:"next,omitempty"`
	NextError       map[string]string `json:"nextError,omitempty"`
	PendingNextLoad bool              `json:"pendingNextLoad"`
}

func placementStatus(e *env, llm *runtime.LLM, src *runtime.HostPlacement, id string, live *livePlacement) placementState {
	req := src.Request(id)
	st := placementState{Model: id, Requested: map[string]any{"mode": req.Mode, "cpuMoeLayers": req.CPUMoELayers, "gpuLayers": req.GPULayers,
		"contextSize": e.Config.CtxSize, "contextMode": e.Config.CtxMode, "contextScope": "all models"}}
	if live != nil && live.Loaded {
		st.Loaded, st.Resolved = true, live.Resolved
		st.PendingNextLoad = live.LoadedRequest.Mode != req.Mode || !sameInt(live.LoadedRequest.CPUMoELayers, req.CPUMoELayers) || !sameInt(live.LoadedRequest.GPULayers, req.GPULayers)
	}
	plan, err := llm.PreviewPlacement(id, req)
	var pe *placement.Error
	switch {
	case err == nil && plan.Placement != nil:
		b, _ := json.Marshal(plan.Placement)
		json.Unmarshal(b, &st.Next)
		st.Next["contextSize"] = plan.CtxSize
	case errors.As(err, &pe):
		st.NextError = map[string]string{"code": pe.Code, "message": pe.Message}
	}
	return st
}

func sameInt(a, b *int) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

type livePlacement struct {
	Loading       bool              `json:"loading"`
	Loaded        bool              `json:"loaded"`
	Resolved      map[string]any    `json:"resolved"`
	LoadedRequest placement.Request `json:"loadedRequest"`
}

// gatewayPlacement asks a running gateway, with its instance token.
func gatewayPlacement(e *env, id string) *livePlacement {
	state, o, _ := e.Root.OwnerState()
	if state != "active" || o == nil {
		return nil
	}
	req, _ := http.NewRequest(http.MethodGet, strings.TrimSuffix(o.HealthURL(), "/health")+"/_localbase/placement-state?model="+id, nil)
	req.Header.Set("X-Localbase-Instance-Token", o.InstanceToken)
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var l livePlacement
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&l) != nil {
		return nil
	}
	return &l
}

// placementTextLines are the human lines: capacity and CPU use only.
func placementTextLines(st placementState) []string {
	mode, _ := st.Requested["mode"].(string)
	lines := []string{"Placement", "  Mode:           " + modeLabel(mode)}
	if st.PendingNextLoad {
		lines[1] += " (applies on next load)"
	}
	describe := func(label string, p map[string]any) {
		note, _ := p["note"].(string)
		lines = append(lines, "  "+label+note)
		gpu, total := num(p["gpuLayers"]), num(p["totalLayers"])
		row := fmt.Sprintf("                  %d of %d layers on the GPU", gpu, total)
		if n := num(p["cpuMoeLayers"]); n > 0 {
			row += fmt.Sprintf("; experts of layers 0–%d (%d tensors) in system memory", n-1, num(p["hostTensors"]))
		}
		if host := num(p["hostBytes"]); host > 0 {
			row += fmt.Sprintf("; %s of system memory", gbText(int64(host)))
		}
		lines = append(lines, row)
		if p["platform"] == placement.Discrete {
			at := ""
			if s, ok := p["plannedAt"].(string); ok && s != "" {
				at = " at " + s
			}
			lines = append(lines, fmt.Sprintf("                  planned%s against %s of free GPU memory (%s kept free)", at, gbText(int64(num(p["freeVramBytes"]))), gbText(placement.ReserveBytes)))
		}
	}
	if st.Loaded && st.Resolved != nil {
		describe("Loaded now:     ", st.Resolved)
	}
	if st.Next != nil {
		describe("Next load:      ", st.Next)
	} else if st.NextError != nil {
		lines = append(lines, "  Next load:      "+st.NextError["message"])
	}
	lines = append(lines, fmt.Sprintf("  Context size:   %v tokens (%v; shared by all models)", st.Requested["contextSize"], st.Requested["contextMode"]))
	return lines
}

func modeLabel(m string) string {
	switch m {
	case placement.GPU:
		return "GPU only"
	case placement.CPU:
		return "CPU only"
	case placement.Custom:
		return "Custom"
	}
	return "Auto"
}

func num(v any) int {
	f, _ := v.(float64)
	if i, ok := v.(int); ok {
		return i
	}
	if i, ok := v.(int64); ok {
		return int(i)
	}
	return int(f)
}

func gbText(b int64) string { return strconv.FormatFloat(float64(b)/(1<<30), 'f', 1, 64) + " GB" }

// showPlacementLines is the Placement block of `models show` ("" on any
// problem: the block is informational).
func showPlacementLines(e *env, id string) []string {
	if e.Store == nil {
		return nil
	}
	lib, err := openLibrary(e)
	if err != nil {
		return nil
	}
	mem := memory.NewController(e.Host, memory.Reserve{Percent: e.Config.SystemReserve.Percent, MinimumGB: e.Config.SystemReserve.MinimumGB}, false)
	src := &runtime.HostPlacement{Store: e.Store, DeviceID: lib.DeviceID, Memory: mem}
	llm := runtime.NewLLM(runtime.LLMDeps{Root: e.Root.Dir, ModelsDir: e.Root.ModelsDir("llm"), RuntimesDir: e.Root.RuntimesDir(),
		Store: e.Store, Catalog: e.Catalog, Memory: mem, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		MemoryGB: float64(e.memoryGB()), Gate: lib.Gate(), Placement: src}, e.Config)
	defer llm.Close()
	return append([]string{""}, placementTextLines(placementStatus(e, llm, src, id, gatewayPlacement(e, id)))...)
}
