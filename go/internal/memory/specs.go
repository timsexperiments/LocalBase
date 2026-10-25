package memory

import (
	"bufio"
	"context"
	"math"
	"os"
	"os/exec"
	"regexp"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Specs is the hardware summary TS detectSpecs reports (doctor, defaults).
type Specs struct {
	OSName         string `json:"osName"`
	RAMGB          int    `json:"ramGb"`
	CPUModel       string `json:"cpuModel"`
	GPUName        string `json:"gpuName"`
	GPUVramGB      int    `json:"gpuVramGb"`
	IsMac          bool   `json:"isMac"`
	IsAppleSilicon bool   `json:"isAppleSilicon"`
	// NVIDIA lists the GPUs nvidia-smi reports (Linux). LocalBase does not
	// admit models against their VRAM yet (DESIGN §11.3, §19): admission
	// uses the system memory pool.
	NVIDIA []NvidiaGPU `json:"nvidiaGpus,omitempty"`
}

// NvidiaGPU is one GPU as nvidia-smi reports it.
type NvidiaGPU struct {
	Name      string `json:"name"`
	MemoryMiB int    `json:"memoryMiB"`
}

// VRAMAdmissionNote is what doctor and the admission log say when an NVIDIA
// GPU is present.
const VRAMAdmissionNote = "NVIDIA GPU present, VRAM admission not qualified on this build"

// nvidiaSMI runs nvidia-smi (tests replace it).
var nvidiaSMI = func() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=name,memory.total", "--format=csv,noheader,nounits").Output()
}

// parseNvidiaSMI reads "name, MiB" lines.
func parseNvidiaSMI(out []byte) []NvidiaGPU {
	var gpus []NvidiaGPU
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name, mem, ok := strings.Cut(line, ",")
		if !ok || strings.TrimSpace(name) == "" {
			continue
		}
		mib, _ := strconv.Atoi(strings.TrimSpace(mem))
		gpus = append(gpus, NvidiaGPU{Name: strings.TrimSpace(name), MemoryMiB: mib})
	}
	return gpus
}

// DetectNVIDIA lists NVIDIA GPUs on Linux (nil elsewhere or without
// nvidia-smi).
func DetectNVIDIA() []NvidiaGPU {
	if goruntime.GOOS != "linux" {
		return nil
	}
	out, err := nvidiaSMI()
	if err != nil {
		return nil
	}
	return parseNvidiaSMI(out)
}

var nvidiaOnce = sync.OnceValue(DetectNVIDIA)

// NVIDIAPresent reports, once per process, whether an NVIDIA GPU is present.
func NVIDIAPresent() bool { return len(nvidiaOnce()) > 0 }

// OverrideNVIDIA makes NVIDIAPresent report gpus until restore (tests).
func OverrideNVIDIA(gpus []NvidiaGPU) (restore func()) {
	prev := nvidiaOnce
	nvidiaOnce = func() []NvidiaGPU { return gpus }
	return func() { nvidiaOnce = prev }
}

var appleChipRe = regexp.MustCompile(`(?i)\bApple\s+M\d+(?:\s+(?:Pro|Max|Ultra))?\b`)

func sysctlString(name string) string {
	out, err := exec.Command("/usr/sbin/sysctl", "-n", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// DetectSpecs reads the host summary. On Linux an NVIDIA GPU is listed
// (nvidia-smi) but its VRAM is not used for admission yet (DESIGN §19), so
// GPUVramGB stays 0.
func DetectSpecs() Specs {
	s := Specs{OSName: "Unknown", CPUModel: "Unknown", GPUName: "Unavailable"}
	switch goruntime.GOOS {
	case "darwin":
		s.IsMac, s.OSName = true, "macOS"
		s.IsAppleSilicon = sysctlString("hw.optional.arm64") == "1" || goruntime.GOARCH == "arm64"
		s.RAMGB = int(math.Round(float64(sysctlInt("hw.memsize")) / float64(GiB)))
		s.CPUModel = sysctlString("machdep.cpu.brand_string")
		if s.CPUModel == "" {
			s.CPUModel = map[bool]string{true: "Apple Silicon", false: "Unknown CPU"}[s.IsAppleSilicon]
		}
		if s.IsAppleSilicon {
			s.GPUName = "Apple Silicon GPU"
			if chip := appleChipRe.FindString(s.CPUModel); chip != "" {
				s.GPUName = chip + " GPU"
			}
			s.GPUVramGB = s.RAMGB
		} else {
			s.GPUName = "Intel Integrated Graphics"
		}
	case "linux":
		s.OSName = "Unknown Linux"
		if b, err := os.ReadFile("/etc/os-release"); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
					if v = strings.Trim(strings.TrimSpace(v), `"'`); v != "" {
						s.OSName = v
					}
				}
			}
		}
		if f, err := os.Open("/proc/meminfo"); err == nil {
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				if fields := strings.Fields(sc.Text()); len(fields) > 1 && fields[0] == "MemTotal:" {
					kb, _ := strconv.ParseFloat(fields[1], 64)
					s.RAMGB = int(math.Round(kb / 1024 / 1024))
				}
			}
			f.Close()
		}
		if f, err := os.Open("/proc/cpuinfo"); err == nil {
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				if k, v, ok := strings.Cut(sc.Text(), ":"); ok && strings.HasPrefix(k, "model name") && strings.TrimSpace(v) != "" {
					s.CPUModel = strings.TrimSpace(v)
					break
				}
			}
			f.Close()
		}
		if s.CPUModel == "Unknown" {
			s.CPUModel = "Unknown CPU"
		}
		s.GPUName = "CPU / Integrated Graphics"
		if s.NVIDIA = DetectNVIDIA(); len(s.NVIDIA) > 0 {
			s.GPUName = s.NVIDIA[0].Name
		}
	default:
		s.OSName = goruntime.GOOS
		s.CPUModel = "Unknown CPU"
	}
	return s
}
