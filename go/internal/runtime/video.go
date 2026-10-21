package runtime

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/install"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// sd-server video runtimes (TS resolveVideoLaunchPlan, the video supervisor
// factory and buildSdVideoServerArgs): a catalog model with a video runtime
// profile, eligible on this device, with every artifact verified, served by
// the pinned sd-server in vid_gen mode.

// videoStartup is TS VIDEO_STARTUP_TIMEOUT_MS.
const videoStartup = 180 * time.Second

// Video eligibility messages (TS model-eligibility.ts).
const (
	unsupportedVideoTarget = "Requires Linux x64 with a single NVIDIA GPU."
	experimentalOptIn      = "Experimental models require models.allowExperimental = true."
)

// VideoTarget names the device a video profile must support (TS
// videoTargetFromTopology); ok is false when no video target runs here.
func VideoTarget(topo memory.Topology) (platform, arch, accelerator string, ok bool) {
	if goruntime.GOOS == "darwin" && goruntime.GOARCH == "arm64" && topo.Unified {
		return "darwin", "arm64", "apple-unified", true
	}
	// The Linux NVIDIA target (one discrete NVIDIA GPU) needs accelerator
	// topology, which arrives with the Linux qualification.
	return "", "", "", false
}

// videoEligible refuses an experimental model without the opt-in and a
// profile that does not support this device (TS assertModelEligible).
func (s *Service) videoEligible(spec *catalog.Spec) error {
	if spec.QualificationState == "experimental" && (s.d.Config == nil || !s.d.Config().AllowExperimental) {
		return errors.New(spec.ModelID + ": " + experimentalOptIn)
	}
	var topo memory.Topology
	if s.d.Memory != nil {
		topo = s.d.Memory.Topology()
	}
	platform, arch, acc, ok := VideoTarget(topo)
	if ok {
		for _, t := range spec.VideoRuntime.SupportedTargets {
			if t.Platform == platform && t.Architecture == arch && t.Accelerator == acc {
				return nil
			}
		}
	}
	return &LaunchError{Code: "model_hardware_ineligible", Message: spec.ModelID + ": " + unsupportedVideoTarget}
}

// sdVideoCommand builds the vid_gen sd-server command.
func (s *Service) sdVideoCommand(t *Target, host string, port int) (*serviceCommand, error) {
	spec := s.d.Catalog.ByID(t.ModelID)
	if spec == nil || t.Imported || spec.Kind != "video" || spec.VideoRuntime == nil {
		return nil, fmt.Errorf("Video model %q has no runtime profile.", t.ModelID)
	}
	if err := s.videoEligible(spec); err != nil {
		return nil, err
	}
	rt := spec.VideoRuntime
	at := func(name string) string { return filepath.Join(t.Dir, name) }
	paths := []string{at(rt.Artifacts.DiffusionModel), at(rt.Artifacts.TextEncoder), at(rt.Artifacts.Decoder.ArtifactFilename)}
	if rt.Mode == "s2v" {
		paths = append(paths, at(rt.Artifacts.AudioEncoder))
	}
	for _, p := range paths {
		if !fileExists(p) {
			return nil, errors.New("Video model artifacts are missing.")
		}
	}
	for _, a := range spec.Artifacts {
		if a.ExpectedSizeBytes == 0 || a.SHA256 == "" {
			return nil, fmt.Errorf("Video model %q lacks immutable artifact authority.", t.ModelID)
		}
		verify := s.d.Verify
		if verify == nil {
			verify = func(path string, a catalog.Artifact, dir string) error {
				_, err := install.Verify(path, a, dir)
				return err
			}
		}
		if err := verify(at(a.Filename), a, t.Dir); err != nil {
			return nil, err
		}
	}
	bin, err := s.binary("sd-server", t)
	if err != nil {
		return nil, fmt.Errorf("%s is not installed on this device: %v", t.Backend.Name, err)
	}
	if bin, err = filepath.Abs(bin); err != nil {
		return nil, err
	}
	decoder := "--tae"
	if rt.Mode == "s2v" || rt.Artifacts.Decoder.Kind == "vae" {
		decoder = "--vae"
	}
	args := []string{"--diffusion-model", paths[0], "--t5xxl", paths[1], decoder, paths[2]}
	if rt.Mode == "s2v" {
		args = append(args, "--audio-encoder", paths[3])
	}
	o := rt.Qualification.LaunchOptions
	if o.CPUOffload {
		args = append(args, "--offload-to-cpu")
	}
	if o.DiffusionFlashAttention {
		args = append(args, "--diffusion-fa")
	}
	if o.VAEConvDirect {
		args = append(args, "--vae-conv-direct")
	}
	args = append(args, "--listen-ip", host, "--listen-port", strconv.Itoa(port))
	if s.d.Memory != nil && !s.d.Memory.Topology().Unified {
		d := s.demand(t)
		args = append(args, "--max-vram", jsNumber(float64(d.AcceleratorBytes)/float64(memory.GiB)))
	}
	env := os.Environ()
	if goruntime.GOOS == "linux" {
		if err := requireSdGPUContract(bin); err != nil {
			return nil, err
		}
	}
	return &serviceCommand{name: "sd-server", bin: bin, dir: filepath.Dir(bin), args: args, env: env, health: "/", startup: videoStartup}, nil
}

// Kill stops the backend under a video job (TS supervisedStop); the next
// job starts it again. The lease stays held until Release.
func (l *ServiceLease) Kill() error {
	s := l.s
	s.mu.Lock()
	b := l.b
	if s.cur == b {
		s.cur = nil
	}
	s.mu.Unlock()
	if b != nil {
		s.stop(b)
	}
	return nil
}

// InstallConsent refuses a catalog model whose files are incomplete when
// installing is not allowed (TS assertModelInstallConsent).
func (s *Service) InstallConsent(modelID string) error {
	spec := s.d.Catalog.ByID(modelID)
	if spec == nil {
		return nil
	}
	if ok, _ := spec.Installation(s.d.ModelsDir); ok || s.d.Install != nil {
		return nil
	}
	return &NotInstalledError{ModelID: modelID}
}

// Config is the runtime configuration the service reads.
func (s *Service) Config() *store.Config { return s.d.Config() }

// BaseURL is where the service's backend listens.
func (s *Service) BaseURL() string {
	return "http://" + net.JoinHostPort(BackendHost(s.d.Host), strconv.Itoa(s.d.Port))
}
