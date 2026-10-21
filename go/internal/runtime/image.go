package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
)

// sd-server (stable-diffusion.cpp) image runtimes: the TS launcher's
// model-specific arguments (buildSdImageServerArgs) and startup budgets.

const (
	imageDefaultStartup = 30 * time.Second
	imageLargeStartup   = 180 * time.Second
	// imageLargeStartupBytes is the weight total, across every component file,
	// at which sd-server gets the long startup budget.
	imageLargeStartupBytes = 10 << 30
)

// ImageLoraPath is the LoRA file a catalog image model applies ("" if none).
func ImageLoraPath(spec *catalog.Spec, modelsDir string) string {
	if spec == nil || spec.ImageLaunchOptions == nil || spec.ImageLaunchOptions.Lora == nil {
		return ""
	}
	p, _ := filepath.Abs(filepath.Join(modelsDir, spec.ImageLaunchOptions.Lora.ArtifactFilename))
	return p
}

func jsNumber(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func generationArgs(g catalog.Generation) []string {
	args := []string{"--sampling-method", g.Sampler}
	if g.Sampler == "lcm" {
		args = append(args, "--scheduler", g.Scheduler)
	}
	return append(args, "--steps", strconv.Itoa(g.Steps), "--cfg-scale", jsNumber(g.CfgScale))
}

// imageModelArgs returns the model arguments and every file they name.
func imageModelArgs(spec *catalog.Spec, dir, weights string) (args, paths []string) {
	var p *catalog.ImageRuntime
	if spec != nil {
		p = spec.ImageRuntime
	}
	if p == nil {
		return []string{"-m", weights}, []string{weights}
	}
	at := func(name string) string { return filepath.Join(dir, name) }
	a := p.Artifacts
	switch p.Kind {
	case "diffusion-qwen3":
		paths = []string{at(a.DiffusionModel), at(a.VAE), at(a.TextEncoder)}
		args = []string{"--diffusion-model", paths[0], "--vae", paths[1], "--llm", paths[2]}
	case "diffusion-flux1":
		paths = []string{at(a.DiffusionModel), at(a.VAE), at(a.ClipL), at(a.T5XXL)}
		args = []string{"--diffusion-model", paths[0], "--vae", paths[1], "--clip_l", paths[2], "--t5xxl", paths[3], "--clip-on-cpu"}
	default: // checkpoint
		paths = []string{at(a.DiffusionModel)}
		args = []string{"-m", paths[0]}
	}
	return append(args, generationArgs(p.Generation)...), paths
}

func (s *Service) sdServerCommand(t *Target, host string, port int, weights string) (*serviceCommand, error) {
	var spec *catalog.Spec
	if s.d.Catalog != nil && !t.Imported {
		spec = s.d.Catalog.ByID(t.ModelID)
	}
	args, paths := imageModelArgs(spec, t.Dir, weights)
	if lora := ImageLoraPath(spec, t.Dir); lora != "" {
		paths = append(paths, lora)
	}
	for _, p := range paths {
		if !fileExists(p) {
			return nil, fmt.Errorf("Image artifact not found: %s", p)
		}
	}
	bin, err := s.binary("sd-server", t)
	if err != nil {
		return nil, fmt.Errorf("%s is not installed on this device: %v", t.Backend.Name, err)
	}
	if bin, err = filepath.Abs(bin); err != nil {
		return nil, err
	}
	if o := spec; o != nil && o.ImageLaunchOptions != nil {
		if o.ImageLaunchOptions.DiffusionFlashAttention {
			args = append(args, "--diffusion-fa")
		}
		if o.ImageLaunchOptions.Prediction != "" {
			args = append(args, "--prediction", o.ImageLaunchOptions.Prediction)
		}
	}
	args = append(args, "--listen-ip", host, "--listen-port", strconv.Itoa(port))
	env := os.Environ()
	if goruntime.GOOS == "linux" {
		if err := requireSdGPUContract(bin); err != nil {
			return nil, err
		}
		ld := filepath.Dir(bin)
		if v := os.Getenv("LD_LIBRARY_PATH"); v != "" {
			ld += string(os.PathListSeparator) + v
		}
		env = append(env, "LD_LIBRARY_PATH="+ld)
	}
	// The budget follows the catalog weight total; other models keep the default.
	startup := imageDefaultStartup
	var total int64
	if spec != nil {
		for _, a := range spec.Artifacts {
			total += a.ExpectedSizeBytes
		}
	}
	if total >= imageLargeStartupBytes {
		startup = imageLargeStartup
	}
	return &serviceCommand{name: "sd-server", bin: bin, dir: filepath.Dir(bin), args: args, env: env, health: "/", startup: startup}, nil
}

// requireSdGPUContract refuses a Linux sd-server without the LocalBase
// GPU-pinning contract (TS requireSdGpuContract); CPU fallback is disabled.
func requireSdGPUContract(bin string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--localbase-capabilities").Output()
	if err == nil && strings.TrimSpace(string(out)) == "localbase-sd-gpu-pci-v1" {
		return nil
	}
	return fmt.Errorf("Unsupported Linux sd-server runtime: --require-gpu-pci is required. Install the LocalBase PCI-capable sd-server release or replace the user-managed sd-server. CPU-only fallback is disabled.")
}
