package memory

import (
	"bufio"
	"math"
	"os"
	"os/exec"
	"regexp"
	goruntime "runtime"
	"strconv"
	"strings"
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
}

var appleChipRe = regexp.MustCompile(`(?i)\bApple\s+M\d+(?:\s+(?:Pro|Max|Ultra))?\b`)

func sysctlString(name string) string {
	out, err := exec.Command("/usr/sbin/sysctl", "-n", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// DetectSpecs reads the host summary. Discrete GPU detection on Linux is
// deferred with the NVIDIA work (DESIGN §11), so Linux reports no GPU.
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
	default:
		s.OSName = goruntime.GOOS
		s.CPUModel = "Unknown CPU"
	}
	return s
}
