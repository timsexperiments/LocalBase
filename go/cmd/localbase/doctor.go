package main

import (
	"fmt"
	"github.com/timsexperiments/LocalBase/go/internal/placement"
	"os"
	"strconv"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/conf"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
)

func doctorCommand() *cli.Command {
	return &cli.Command{Name: "doctor", Summary: "Run a system health check and print configuration details",
		Run: func(c *cli.Ctx) (any, error) {
			e, err := readEnv(c, true)
			if err != nil {
				return nil, err
			}
			defer e.Close()
			if err := applyEnvOverrides(e.Config); err != nil {
				return nil, err
			}
			s := memory.DetectSpecs()
			c.Info("OS: %s", s.OSName)
			c.Info("CPU: %s", s.CPUModel)
			c.Info("RAM: %d GB", s.RAMGB)
			for _, l := range gpuLines(s, memory.GPUMemoryNow()) {
				c.Info("%s", l)
			}
			c.Info("Parallel Slots: %s", e.Config.Parallel)
			eff, err := obs.ResolveOtel(e.Config.OtelEndpoint, e.Config.OtelHeaders, e.Config.OtelSampleRatio, os.Getenv)
			if err != nil {
				return nil, err
			}
			if eff.Enabled {
				c.Info("OpenTelemetry: enabled via %s (%s)", eff.Source, eff.DisplayEndpoint)
			} else {
				c.Info("OpenTelemetry: disabled")
			}
			switch {
			case s.IsAppleSilicon && s.RAMGB >= 64:
				c.Info("Status: ✅ Excellent fit for large developer-grade models (up to 32B/70B) using unified memory.")
			case s.IsAppleSilicon && s.RAMGB >= 32:
				c.Info("Status: ✅ Good fit for medium coding models (up to 14B/32B) using unified memory.")
			case s.IsAppleSilicon && s.RAMGB >= 16:
				c.Info("Status: ⚠️ Good for entry coding models (1.5B/7B), but larger models may slow down.")
			case s.IsAppleSilicon:
				c.Info("Status: ❌ Low memory. Limit to small models (1.5B) and small contexts.")
			case len(s.NVIDIA) > 0:
				c.Info("Status: ✅ Discrete NVIDIA GPU with %d GB of system memory: models are placed against the GPU's free memory, and what does not fit stays in system memory.", s.RAMGB)
			case s.GPUVramGB >= 16:
				c.Info("Status: ✅ Excellent fit for multi-model coding setup (7B/14B models + local STT).")
			case s.GPUVramGB >= 12:
				c.Info("Status: ✅ Good fit for 7B models with plenty of context headroom.")
			case s.GPUVramGB >= 8:
				c.Info("Status: ⚠️ Tight fit for 7B models (warning: context growth may slow down), or perfect for 3B/1.5B models.")
			default:
				c.Info("Status: ❌ Low VRAM. Focus on 1.5B/3B models or CPU offloading.")
			}
			data := map[string]any{"hardware": s, "configuration": conf.Public(e.Config, eff)}
			if st := obs.InspectServiceStderr(e.Root.Dir); st != nil {
				archive := ""
				if st.ArchiveBytes > 0 {
					archive = "; " + strconv.FormatInt(st.ArchiveBytes, 10) + " bytes in the .1 archive"
				}
				c.Info("Service stderr: %d bytes captured at %s (last written %s%s). It holds unredacted gateway crash output; review it before sharing.",
					st.Bytes, st.Path, st.ModifiedAt, archive)
				data["serviceStderr"] = st
			}
			return data, nil
		}}
}

// gpuLines are doctor's GPU lines, with where CPU/GPU placement applies
// (DESIGN §20): planned against an NVIDIA GPU's free memory, not applicable
// on unified memory.
func gpuLines(s memory.Specs, gpus []memory.GPUMemory) []string {
	gb := func(b int64) string { return strconv.FormatFloat(float64(b)/(1<<30), 'f', 1, 64) + " GB" }
	lines := []string{"GPU: " + s.GPUName}
	switch {
	case len(s.NVIDIA) > 0:
		for i, g := range s.NVIDIA {
			line := fmt.Sprintf("NVIDIA GPU %d: %s, %s", i, g.Name, gb(int64(g.MemoryMiB)<<20))
			if i < len(gpus) {
				line = fmt.Sprintf("NVIDIA GPU %d: %s, %s free of %s", gpus[i].Index, gpus[i].Name, gb(gpus[i].FreeBytes), gb(gpus[i].TotalBytes))
			}
			lines = append(lines, line)
		}
		if len(s.NVIDIA) > 1 {
			lines = append(lines, fmt.Sprintf("Placement: %d NVIDIA GPUs; LocalBase places a model on one. Set CUDA_VISIBLE_DEVICES to a single GPU (for example CUDA_VISIBLE_DEVICES=0).", len(s.NVIDIA)))
		} else {
			lines = append(lines, "Placement: discrete NVIDIA GPU; models are planned against its free memory ("+gb(placement.ReserveBytes)+" kept free), and what does not fit stays in system memory.")
		}
	case s.IsAppleSilicon:
		lines = append(lines, fmt.Sprintf("GPU VRAM: %d GB (Unified Memory)", s.GPUVramGB),
			"Placement: unified memory; the GPU and CPU share it, so placement planning does not apply.")
	default:
		lines = append(lines, fmt.Sprintf("GPU VRAM: %d GB (Discrete Memory)", s.GPUVramGB), "Placement: no GPU found; models run on the CPU.")
	}
	return lines
}
